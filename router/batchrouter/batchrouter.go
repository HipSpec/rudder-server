package batchrouter

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rudderlabs/rudder-server/config"
	backendconfig "github.com/rudderlabs/rudder-server/config/backend-config"
	"github.com/rudderlabs/rudder-server/jobsdb"
	warehouseutils "github.com/rudderlabs/rudder-server/router/warehouse/utils"
	"github.com/rudderlabs/rudder-server/services/filemanager"
	"github.com/rudderlabs/rudder-server/services/stats"
	"github.com/rudderlabs/rudder-server/utils"
	"github.com/rudderlabs/rudder-server/utils/logger"
	"github.com/rudderlabs/rudder-server/utils/misc"
	uuid "github.com/satori/go.uuid"
	"github.com/tidwall/gjson"
)

var (
	jobQueryBatchSize         int
	noOfWorkers               int
	mainLoopSleepInS          int
	batchDestinations         []DestinationT
	configSubscriberLock      sync.RWMutex
	objectStorageDestinations []string
	warehouseDestinations     []string
	inProgressMap             map[string]bool
	inProgressMapLock         sync.RWMutex
	uploadedRawDataJobsCache  map[string]bool
	errorsCountStat           *stats.RudderStats
	warehouseJSONUploadsTable string
)

type HandleT struct {
	processQ     chan BatchJobsT
	jobsDB       *jobsdb.HandleT
	jobsDBHandle *sql.DB
	isEnabled    bool
}

type ObjectStorageT struct {
	Bucket   string
	Key      string
	Provider string
}

func backendConfigSubscriber() {
	ch := make(chan utils.DataEvent)
	backendconfig.Subscribe(ch)
	for {
		config := <-ch
		configSubscriberLock.Lock()
		batchDestinations = []DestinationT{}
		allSources := config.Data.(backendconfig.SourcesT)
		for _, source := range allSources.Sources {
			if source.Enabled && len(source.Destinations) > 0 {
				for _, destination := range source.Destinations {
					if destination.Enabled && (misc.Contains(objectStorageDestinations, destination.DestinationDefinition.Name) || misc.Contains(warehouseDestinations, destination.DestinationDefinition.Name)) {
						batchDestinations = append(batchDestinations, DestinationT{Source: source, Destination: destination})
					}
				}
			}
		}
		configSubscriberLock.Unlock()
	}
}

type StorageUploadOutput struct {
	Bucket         string
	Key            string
	LocalFilePaths []string
	Error          error
	JournalOpID    int64
}

type ErrorResponseT struct {
	Error string
}

func updateDestStatusStats(id string, count int, isSuccess bool) {
	var destStatsD *stats.RudderStats
	if isSuccess {
		destStatsD = stats.NewBatchDestStat("batch_router.dest_successful_events", stats.CountType, id)
	} else {
		destStatsD = stats.NewBatchDestStat("batch_router.dest_failed_attempts", stats.CountType, id)
		errorsCountStat.Count(count)
	}
	destStatsD.Count(count)
}

func (brt *HandleT) copyJobsToStorage(provider string, batchJobs BatchJobsT, makeJournalEntry bool, isWarehouse bool) StorageUploadOutput {
	var bucketName, dirName string
	if isWarehouse {
		bucketName = config.GetString("WAREHOUSE_JSON_UPLOADS_BUCKET", "rl-redshift-json-dump")
		dirName = "/rudder-warehouse-json-uploads/"
	} else {
		bucketName = batchJobs.BatchDestination.Destination.Config.(map[string]interface{})["bucketName"].(string)
		dirName = "/rudder-raw-data-destination-logs/"
	}

	uuid := uuid.NewV4()
	logger.Debugf("BRT: Starting logging to %s: %s\n", provider, bucketName)

	tmpDirPath := misc.CreateTMPDIR()
	path := fmt.Sprintf("%v%v.json", tmpDirPath+dirName, fmt.Sprintf("%v.%v.%v", time.Now().Unix(), batchJobs.BatchDestination.Source.ID, uuid))

	var contentSlice [][]byte
	for _, job := range batchJobs.Jobs {
		eventID := gjson.GetBytes(job.EventPayload, "messageId").String()
		var ok bool
		if _, ok = uploadedRawDataJobsCache[eventID]; !ok {
			contentSlice = append(contentSlice, job.EventPayload)
		}
	}
	content := bytes.Join(contentSlice[:], []byte("\n"))

	gzipFilePath := fmt.Sprintf(`%v.gz`, path)
	err := os.MkdirAll(filepath.Dir(gzipFilePath), os.ModePerm)
	misc.AssertError(err)
	gzipFile, err := os.Create(gzipFilePath)

	gzipWriter := gzip.NewWriter(gzipFile)
	_, err = gzipWriter.Write(content)
	misc.AssertError(err)
	gzipWriter.Close()

	logger.Debugf("BRT: Logged to local file: %v\n", gzipFilePath)

	uploader, err := filemanager.New(&filemanager.SettingsT{
		Provider: provider,
		Bucket:   bucketName,
	})
	gzipFile, err = os.Open(gzipFilePath)
	misc.AssertError(err)

	logger.Debugf("BRT: Starting upload to %s: %s\n", provider, bucketName)

	var keyPrefixes []string
	if isWarehouse {
		keyPrefixes = []string{config.GetEnv("WAREHOUSE_BUCKET_FOLDER_NAME", "rudder-warehouse-logs"), batchJobs.BatchDestination.Source.ID, time.Now().Format("01-02-2006")}
	} else {
		keyPrefixes = []string{config.GetEnv("DESTINATION_BUCKET_FOLDER_NAME", "rudder-logs"), batchJobs.BatchDestination.Source.ID, time.Now().Format("01-02-2006")}
	}

	_, fileName := filepath.Split(gzipFilePath)
	opPayload, err := json.Marshal(&ObjectStorageT{
		Bucket:   bucketName,
		Key:      strings.Join(append(keyPrefixes, fileName), "/"),
		Provider: provider,
	})
	opID := brt.jobsDB.JournalMarkStart(jobsdb.RawDataDestUploadOperation, opPayload)
	_, err = uploader.Upload(gzipFile, keyPrefixes...)

	if err != nil {
		logger.Debug(err)
		return StorageUploadOutput{
			Error:       err,
			JournalOpID: opID,
		}
	}

	return StorageUploadOutput{
		Bucket:         bucketName,
		Key:            strings.Join(keyPrefixes, "/") + "/" + fileName,
		LocalFilePaths: []string{gzipFilePath},
		JournalOpID:    opID,
	}
}

func (brt *HandleT) updateWarehouseMetadata(batchJobs BatchJobsT, location string) (err error) {
	schemaMap := make(map[string]map[string]interface{})
	for _, job := range batchJobs.Jobs {
		var payload map[string]interface{}
		err := json.Unmarshal(job.EventPayload, &payload)
		misc.AssertError(err)
		tableName := payload["metadata"].(map[string]interface{})["table"].(string)
		var ok bool
		if _, ok = schemaMap[tableName]; !ok {
			schemaMap[tableName] = make(map[string]interface{})
		}
		columns := payload["metadata"].(map[string]interface{})["columns"].(map[string]interface{})
		for columnName, columnVal := range columns {
			if _, ok := schemaMap[tableName][columnName]; !ok {
				schemaMap[tableName][columnName] = columnVal
			}
		}
	}
	logger.Debugf("Creating record for uploaded json in %s table with schema: %+v\n", warehouseJSONUploadsTable, schemaMap)
	schemaPayload, err := json.Marshal(schemaMap)
	sqlStatement := fmt.Sprintf(`INSERT INTO %s (location, schema, source_id, status, created_at)
									   VALUES ($1, $2, $3, $4, $5)`, warehouseJSONUploadsTable)
	stmt, err := brt.jobsDBHandle.Prepare(sqlStatement)
	misc.AssertError(err)
	defer stmt.Close()

	_, err = stmt.Exec(location, schemaPayload, batchJobs.BatchDestination.Source.ID, warehouseutils.JSONProcessWaitingState, time.Now())
	misc.AssertError(err)
	return err
}

func (brt *HandleT) setJobStatus(batchJobs BatchJobsT, isWarehouse bool, err error) {
	var (
		jobState   string
		errorResp  []byte
		bucketName string
	)
	if isWarehouse {
		bucketName = config.GetString("WAREHOUSE_JSON_UPLOADS_BUCKET", "rl-redshift-json-dump")
	} else {
		bucketName = batchJobs.BatchDestination.Destination.Config.(map[string]interface{})["bucketName"].(string)
	}
	if err != nil {
		logger.Errorf("BRT: Error uploading to object storage: %v", err)
		jobState = jobsdb.FailedState
		errorResp, _ = json.Marshal(ErrorResponseT{Error: err.Error()})
		// We keep track of number of failed attempts in case of failure and number of events uploaded in case of success in stats
		updateDestStatusStats(batchJobs.BatchDestination.Destination.ID, 1, false)
	} else {
		logger.Debugf("BRT: Uploaded to object storage bucket: %v %v %v\n", bucketName, batchJobs.BatchDestination.Source.ID, time.Now().Format("01-02-2006"))
		jobState = jobsdb.SucceededState
		errorResp = []byte(`{"success":"OK"}`)
		updateDestStatusStats(batchJobs.BatchDestination.Destination.ID, len(batchJobs.Jobs), true)
	}

	var statusList []*jobsdb.JobStatusT

	//Identify jobs which can be processed
	for _, job := range batchJobs.Jobs {
		status := jobsdb.JobStatusT{
			JobID:         job.JobID,
			AttemptNum:    job.LastJobStatus.AttemptNum,
			JobState:      jobState,
			ExecTime:      time.Now(),
			RetryTime:     time.Now(),
			ErrorCode:     "",
			ErrorResponse: errorResp,
		}
		statusList = append(statusList, &status)
	}

	//Mark the jobs as executing
	brt.jobsDB.UpdateJobStatus(statusList, []string{batchJobs.BatchDestination.Destination.DestinationDefinition.Name}, batchJobs.BatchDestination.Source.ID)
}

func (brt *HandleT) initWorkers() {
	for i := 0; i < noOfWorkers; i++ {
		go func() {
			for {
				select {
				case batchJobs := <-brt.processQ:
					destName := batchJobs.BatchDestination.Destination.DestinationDefinition.Name
					switch {
					case misc.ContainsString(objectStorageDestinations, destName):
						destUploadStat := stats.NewStat("batch_router.S3_dest_upload_time", stats.TimerType)
						destUploadStat.Start()
						output := brt.copyJobsToStorage(destName, batchJobs, true, false)
						brt.setJobStatus(batchJobs, false, output.Error)
						brt.jobsDB.JournalDeleteEntry(output.JournalOpID)
						misc.RemoveFilePaths(output.LocalFilePaths...)
						destUploadStat.End()
						setSourceInProgress(batchJobs.BatchDestination, false)
					case misc.ContainsString(warehouseDestinations, destName):
						output := brt.copyJobsToStorage(warehouseutils.ObjectStorageMap[destName], batchJobs, true, true)
						if output.Error == nil {
							brt.updateWarehouseMetadata(batchJobs, output.Key)
						}
						brt.setJobStatus(batchJobs, true, output.Error)
						brt.jobsDB.JournalDeleteEntry(output.JournalOpID)
						misc.RemoveFilePaths(output.LocalFilePaths...)
						setSourceInProgress(batchJobs.BatchDestination, false)
					}
				}
			}
		}()
	}
}

type DestinationT struct {
	Source      backendconfig.SourceT
	Destination backendconfig.DestinationT
}

type BatchJobsT struct {
	Jobs             []*jobsdb.JobT
	BatchDestination DestinationT
}

func isSourceInProgress(batchDestination DestinationT) bool {
	inProgressMapLock.RLock()
	if inProgressMap[batchDestination.Source.ID+"_"+batchDestination.Destination.ID] {
		inProgressMapLock.RUnlock()
		return true
	}
	inProgressMapLock.RUnlock()
	return false
}

func setSourceInProgress(batchDestination DestinationT, starting bool) {
	inProgressMapLock.Lock()
	if starting {
		inProgressMap[batchDestination.Source.ID+"_"+batchDestination.Destination.ID] = true
	} else {
		delete(inProgressMap, batchDestination.Source.ID+"_"+batchDestination.Destination.ID)
	}
	inProgressMapLock.Unlock()
}

func (brt *HandleT) mainLoop() {
	for {
		if !brt.isEnabled {
			time.Sleep(time.Duration(2*mainLoopSleepInS) * time.Second)
			continue
		}
		time.Sleep(time.Duration(mainLoopSleepInS) * time.Second)
		for _, batchDestination := range batchDestinations {
			if isSourceInProgress(batchDestination) {
				continue
			}
			setSourceInProgress(batchDestination, true)
			toQuery := jobQueryBatchSize
			retryList := brt.jobsDB.GetToRetry([]string{batchDestination.Destination.DestinationDefinition.Name}, toQuery, batchDestination.Source.ID)
			toQuery -= len(retryList)
			waitList := brt.jobsDB.GetWaiting([]string{batchDestination.Destination.DestinationDefinition.Name}, toQuery, batchDestination.Source.ID) //Jobs send to waiting state
			toQuery -= len(waitList)
			unprocessedList := brt.jobsDB.GetUnprocessed([]string{batchDestination.Destination.DestinationDefinition.Name}, toQuery, batchDestination.Source.ID)
			if len(waitList)+len(unprocessedList)+len(retryList) == 0 {
				setSourceInProgress(batchDestination, false)
				continue
			}

			combinedList := append(waitList, append(unprocessedList, retryList...)...)

			var statusList []*jobsdb.JobStatusT

			//Identify jobs which can be processed
			for _, job := range combinedList {
				status := jobsdb.JobStatusT{
					JobID:         job.JobID,
					AttemptNum:    job.LastJobStatus.AttemptNum + 1,
					JobState:      jobsdb.ExecutingState,
					ExecTime:      time.Now(),
					RetryTime:     time.Now(),
					ErrorCode:     "",
					ErrorResponse: []byte(`{}`), // check
				}
				statusList = append(statusList, &status)
			}

			//Mark the jobs as executing
			brt.jobsDB.UpdateJobStatus(statusList, []string{batchDestination.Destination.DestinationDefinition.Name}, batchDestination.Source.ID)
			brt.processQ <- BatchJobsT{Jobs: combinedList, BatchDestination: batchDestination}
		}
	}
}

//Enable enables a router :)
func (brt *HandleT) Enable() {
	brt.isEnabled = true
}

//Disable disables a router:)
func (brt *HandleT) Disable() {
	brt.isEnabled = false
}

func (brt *HandleT) dedupRawDataDestJobsOnCrash() {
	logger.Debugf("BRT: Checking for incomplete journal entries to recover from...")
	entries := brt.jobsDB.GetJournalEntries(jobsdb.RawDataDestUploadOperation)
	for _, entry := range entries {
		var object ObjectStorageT
		err := json.Unmarshal(entry.OpPayload, &object)
		misc.AssertError(err)
		downloader, err := filemanager.New(&filemanager.SettingsT{
			Provider: object.Provider,
			Bucket:   object.Bucket,
		})

		dirName := "/rudder-raw-data-dest-upload-crash-recovery/"
		tmpDirPath := misc.CreateTMPDIR()
		jsonPath := fmt.Sprintf("%v%v.json", tmpDirPath+dirName, fmt.Sprintf("%v.%v", time.Now().Unix(), uuid.NewV4().String()))

		err = os.MkdirAll(filepath.Dir(jsonPath), os.ModePerm)
		jsonFile, err := os.Create(jsonPath)
		misc.AssertError(err)

		logger.Debugf("BRT: Downloading data for incomplete journal entry to recover from %s in bucket: %s at key: %s", object.Provider, object.Bucket, object.Key)
		err = downloader.Download(jsonFile, object.Key)
		if err != nil {
			logger.Debugf("BRT: Failed to download data for incomplete journal entry to recover from %s in bucket: %s at key: %s with error: %v", object.Provider, object.Bucket, object.Key, err)
			continue
		}

		jsonFile.Close()
		defer os.Remove(jsonPath)

		rawf, err := os.Open(jsonPath)
		reader, _ := gzip.NewReader(rawf)

		sc := bufio.NewScanner(reader)

		logger.Debugf("BRT: Setting go map cache for incomplete journal entry to recover from...")
		for sc.Scan() {
			lineBytes := sc.Bytes()
			eventID := gjson.GetBytes(lineBytes, "messageId").String()
			uploadedRawDataJobsCache[eventID] = true
		}
	}
}

func (brt *HandleT) crashRecover() {

	for {
		execList := brt.jobsDB.GetExecuting([]string{}, jobQueryBatchSize)

		if len(execList) == 0 {
			break
		}
		logger.Debug("BRT: Batch Router crash recovering", len(execList))

		var statusList []*jobsdb.JobStatusT

		for _, job := range execList {
			status := jobsdb.JobStatusT{
				JobID:         job.JobID,
				AttemptNum:    job.LastJobStatus.AttemptNum,
				ExecTime:      time.Now(),
				RetryTime:     time.Now(),
				JobState:      jobsdb.FailedState,
				ErrorCode:     "",
				ErrorResponse: []byte(`{}`), // check
			}
			statusList = append(statusList, &status)
		}
		brt.jobsDB.UpdateJobStatus(statusList, []string{})
	}
	brt.dedupRawDataDestJobsOnCrash()
}

func (brt *HandleT) setupWarehouseJSONUploadsTable() {

	sqlStatement := `DO $$ BEGIN
                                CREATE TYPE wh_json_upload_state_type
                                     AS ENUM(
                                              'waiting',
                                              'executing',
											  'failed',
											  'succeeded');
                                     EXCEPTION
                                        WHEN duplicate_object THEN null;
                            END $$;`

	_, err := brt.jobsDBHandle.Exec(sqlStatement)
	misc.AssertError(err)

	sqlStatement = fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
                                      id BIGSERIAL PRIMARY KEY,
									  location TEXT NOT NULL,
									  source_id VARCHAR(64) NOT NULL,
									  schema JSONB NOT NULL,
									  status wh_json_upload_state_type,
									  created_at TIMESTAMP NOT NULL);`, warehouseJSONUploadsTable)

	_, err = brt.jobsDBHandle.Exec(sqlStatement)
	misc.AssertError(err)
}

func loadConfig() {
	jobQueryBatchSize = config.GetInt("BatchRouter.jobQueryBatchSize", 100000)
	noOfWorkers = config.GetInt("BatchRouter.noOfWorkers", 8)
	mainLoopSleepInS = config.GetInt("BatchRouter.mainLoopSleepInS", 5)
	warehouseJSONUploadsTable = config.GetString("Warehouse.jsonUploadsTable", "wh_json_uploads")
	objectStorageDestinations = []string{"S3", "GCS"}
	warehouseDestinations = []string{"RS", "BQ"}
	inProgressMap = map[string]bool{}
}

func init() {
	config.Initialize()
	loadConfig()
	uploadedRawDataJobsCache = make(map[string]bool)
	errorsCountStat = stats.NewStat("batch_router.errors", stats.CountType)
}

//Setup initializes this module
func (brt *HandleT) Setup(jobsDB *jobsdb.HandleT) {
	logger.Info("BRT: Batch Router started")
	brt.jobsDB = jobsDB
	brt.jobsDBHandle = brt.jobsDB.GetDBHandle()
	brt.setupWarehouseJSONUploadsTable()
	brt.processQ = make(chan BatchJobsT)
	brt.crashRecover()

	go brt.initWorkers()
	go backendConfigSubscriber()
	go brt.mainLoop()
}
