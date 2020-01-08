package redshift

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/rudderlabs/rudder-server/config"
	warehouseutils "github.com/rudderlabs/rudder-server/router/warehouse/utils"
	"github.com/rudderlabs/rudder-server/services/filemanager"
	"github.com/rudderlabs/rudder-server/services/stats"
	"github.com/rudderlabs/rudder-server/utils/logger"
	"github.com/rudderlabs/rudder-server/utils/misc"
	uuid "github.com/satori/go.uuid"
)

var (
	warehouseUploadsTable string
)

type HandleT struct {
	DbHandle      *sql.DB
	Db            *sql.DB
	Namespace     string
	CurrentSchema map[string]map[string]string
	Warehouse     warehouseutils.WarehouseT
	Upload        warehouseutils.UploadT
}

var dataTypesMap = map[string]string{
	"boolean":  "boolean",
	"int":      "double precision",
	"bigint":   "double precision",
	"float":    "double precision",
	"string":   "varchar(512)",
	"datetime": "timestamp",
}

var primaryKeyMap = map[string]string{
	"users":      "id",
	"identifies": "id",
}

func columnsWithDataTypes(columns map[string]string, prefix string) string {
	arr := []string{}
	for name, dataType := range columns {
		arr = append(arr, fmt.Sprintf(`%s%s %s`, prefix, name, dataTypesMap[dataType]))
	}
	return strings.Join(arr[:], ",")
}

func (rs *HandleT) createTable(name string, columns map[string]string) (err error) {
	sortKeyField := "received_at"
	sqlStatement := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s ( %v ) SORTKEY(%s)`, name, columnsWithDataTypes(columns, ""), sortKeyField)
	_, err = rs.Db.Exec(sqlStatement)
	return
}

func (rs *HandleT) addColumn(tableName string, columnName string, columnType string) (err error) {
	_, err = rs.Db.Exec(fmt.Sprintf(`ALTER TABLE %v ADD COLUMN %s %s`, tableName, columnName, dataTypesMap[columnType]))
	return
}

func (rs *HandleT) createSchema() (err error) {
	// TODO: Change to use source_schema_name in wh_schemas table
	_, err = rs.Db.Exec(fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, rs.Upload.Namespace))
	return
}

func (rs *HandleT) updateSchema() (updatedSchema map[string]map[string]string, err error) {
	diff := warehouseutils.GetSchemaDiff(rs.CurrentSchema, rs.Upload.Schema)
	updatedSchema = diff.UpdatedSchema
	if len(rs.CurrentSchema) == 0 {
		err = rs.createSchema()
		if err != nil {
			return nil, err
		}
	}
	processedTables := make(map[string]bool)
	for _, tableName := range diff.Tables {
		err = rs.createTable(fmt.Sprintf(`%s.%s`, rs.Upload.Namespace, tableName), diff.ColumnMaps[tableName])
		if err != nil {
			return nil, err
		}
		processedTables[tableName] = true
	}
	for tableName, columnMap := range diff.ColumnMaps {
		if _, ok := processedTables[tableName]; ok {
			continue
		}
		if len(columnMap) > 0 {
			for columnName, columnType := range columnMap {
				err := rs.addColumn(fmt.Sprintf(`%s.%s`, rs.Upload.Namespace, tableName), columnName, columnType)
				if !checkAndIgnoreAlreadyExistError(err) {
					return nil, err
				}
			}
		}
	}
	return
}

func checkAndIgnoreAlreadyExistError(err error) bool {
	if err != nil {
		if e, ok := err.(*pq.Error); ok {
			if e.Code == "42701" {
				return true
			}
		}
		return false
	}
	return true
}

type S3ManifestEntryT struct {
	Url       string `json:"url"`
	Mandatory bool   `json:"mandatory"`
}

type S3ManifestT struct {
	Entries []S3ManifestEntryT `json:"entries"`
}

func (rs *HandleT) generateManifest(bucketName, tableName string, columnMap map[string]string) (string, error) {
	csvObjectLocations, err := warehouseutils.GetLoadFileLocations(rs.DbHandle, rs.Warehouse.Source.ID, rs.Warehouse.Destination.ID, tableName, rs.Upload.StartLoadFileID, rs.Upload.EndLoadFileID)
	misc.AssertError(err)
	csvS3Locations, err := warehouseutils.GetS3Locations(csvObjectLocations)
	var manifest S3ManifestT
	for _, location := range csvS3Locations {
		manifest.Entries = append(manifest.Entries, S3ManifestEntryT{Url: location, Mandatory: true})
	}
	manifestJSON, err := json.Marshal(&manifest)

	manifestFolder := "rudder-redshift-manifests"
	dirName := "/" + manifestFolder + "/"
	tmpDirPath := misc.CreateTMPDIR()
	localManifestPath := fmt.Sprintf("%v%v", tmpDirPath+dirName, uuid.NewV4().String())
	err = os.MkdirAll(filepath.Dir(localManifestPath), os.ModePerm)
	misc.AssertError(err)
	_ = ioutil.WriteFile(localManifestPath, manifestJSON, 0644)

	file, err := os.Open(localManifestPath)
	misc.AssertError(err)
	defer file.Close()

	uploader, err := filemanager.New(&filemanager.SettingsT{
		Provider: "S3",
		Config:   map[string]interface{}{"bucketName": bucketName},
	})

	uploadOutput, err := uploader.Upload(file, manifestFolder, rs.Warehouse.Source.ID, rs.Warehouse.Destination.ID, time.Now().Format("01-02-2006"), tableName, uuid.NewV4().String())

	if err != nil {
		return "", err
	}

	return uploadOutput.Location, nil
}

func (rs *HandleT) dropStagingTables(stagingTableNames []string) {
	for _, stagingTableName := range stagingTableNames {
		_, err := rs.Db.Exec(fmt.Sprintf(`DROP TABLE %[1]s."%[2]s"`, rs.Upload.Namespace, stagingTableName))
		misc.AssertError(err)
	}
}

func (rs *HandleT) load() (err error) {
	var accessKeyID, accessKey, bucketName string
	config := rs.Warehouse.Destination.Config.(map[string]interface{})
	if config["accessKeyID"] != nil {
		accessKeyID = config["accessKeyID"].(string)
	}
	if config["accessKey"] != nil {
		accessKey = config["accessKey"].(string)
	}
	if config["bucketName"] != nil {
		bucketName = config["bucketName"].(string)
	}

	// BEGIN TRANSACTION
	tx, err := rs.Db.Begin()
	if err != nil {
		return err
	}

	wg := misc.NewWaitGroup()
	wg.Add(len(rs.Upload.Schema))
	stagingTableNames := []string{}

	for tName, cMap := range rs.Upload.Schema {
		go func(tableName string, columnMap map[string]string) {
			timer := warehouseutils.DestStat(stats.TimerType, "generate_manifest_time", rs.Warehouse.Destination.ID)
			timer.Start()
			manifestLocation, err := rs.generateManifest(bucketName, tableName, columnMap)
			timer.End()
			if err != nil {
				wg.Err(err)
				return
			}

			// sort columnnames
			keys := reflect.ValueOf(columnMap).MapKeys()
			strkeys := make([]string, len(keys))
			for i := 0; i < len(keys); i++ {
				strkeys[i] = keys[i].String()
			}
			sort.Strings(strkeys)
			sortedColumnNames := strings.Join(strkeys, ",")

			stagingTableName := "staging-" + tableName + "-" + uuid.NewV4().String()
			err = rs.createTable(fmt.Sprintf(`%s."%s"`, rs.Upload.Namespace, stagingTableName), rs.Upload.Schema[tableName])
			if err != nil {
				wg.Err(err)
				return
			}
			stagingTableNames = append(stagingTableNames, stagingTableName)

			region, manifestS3Location := warehouseutils.GetS3Location(manifestLocation)
			if region == "" {
				region = "us-east-1"
			}
			sqlStatement := fmt.Sprintf(`COPY %v(%v) FROM '%v' CSV GZIP ACCESS_KEY_ID '%s' SECRET_ACCESS_KEY '%s' REGION '%s'  DATEFORMAT 'auto' TIMEFORMAT 'auto' MANIFEST TRUNCATECOLUMNS EMPTYASNULL BLANKSASNULL FILLRECORD ACCEPTANYDATE TRIMBLANKS ACCEPTINVCHARS COMPUPDATE OFF `, fmt.Sprintf(`%s."%s"`, rs.Upload.Namespace, stagingTableName), sortedColumnNames, manifestS3Location, accessKeyID, accessKey, region)

			_, err = tx.Exec(sqlStatement)
			if err != nil {
				wg.Err(err)
				return
			}

			primaryKey := "id"
			if column, ok := primaryKeyMap[tableName]; ok {
				primaryKey = column
			}

			sqlStatement = fmt.Sprintf(`delete from %[1]s."%[2]s" using %[1]s."%[3]s" _source where _source.%[4]s = %[1]s.%[2]s.%[4]s`, rs.Upload.Namespace, tableName, stagingTableName, primaryKey)
			_, err = tx.Exec(sqlStatement)
			if err != nil {
				wg.Err(err)
				return
			}

			var quotedColumnNames string
			for idx, str := range strkeys {
				quotedColumnNames += "\"" + str + "\""
				if idx != len(strkeys)-1 {
					quotedColumnNames += ","
				}
			}

			sqlStatement = fmt.Sprintf(`INSERT INTO %[1]s."%[2]s" (%[3]s) SELECT %[3]s FROM ( SELECT *, row_number() OVER (PARTITION BY %[5]s ORDER BY received_at ASC) AS _rudder_staging_row_number FROM %[1]s."%[4]s" ) AS _ where _rudder_staging_row_number = 1`, rs.Upload.Namespace, tableName, quotedColumnNames, stagingTableName, primaryKey)
			_, err = tx.Exec(sqlStatement)
			if err != nil {
				wg.Err(err)
				return
			}
			wg.Done()
		}(tName, cMap)
	}

	err = wg.Wait()
	defer rs.dropStagingTables(stagingTableNames)
	if err != nil {
		tx.Rollback()
		return err
	}

	err = tx.Commit()
	return err
}

// RedshiftCredentialsT ...
type RedshiftCredentialsT struct {
	host     string
	port     string
	dbName   string
	username string
	password string
}

func connect(cred RedshiftCredentialsT) (*sql.DB, error) {
	url := fmt.Sprintf("sslmode=require user=%v password=%v host=%v port=%v dbname=%v",
		cred.username,
		cred.password,
		cred.host,
		cred.port,
		cred.dbName)

	var err error
	var db *sql.DB
	if db, err = sql.Open("postgres", url); err != nil {
		return nil, fmt.Errorf("redshift connect error : (%v)", err)
	}
	return db, nil
}

func loadConfig() {
	warehouseUploadsTable = config.GetString("Warehouse.uploadsTable", "wh_uploads")
}

func init() {
	config.Initialize()
	loadConfig()
}

func (rs *HandleT) MigrateSchema() (err error) {
	timer := warehouseutils.DestStat(stats.TimerType, "migrate_schema_time", rs.Warehouse.Destination.ID)
	timer.Start()
	warehouseutils.SetUploadStatus(rs.Upload, warehouseutils.UpdatingSchemaState, rs.DbHandle)
	logger.Debugf("RS: Updaing schema for redshfit schemaname: %s\n", rs.Upload.Namespace)
	updatedSchema, err := rs.updateSchema()
	if err != nil {
		warehouseutils.SetUploadError(rs.Upload, err, warehouseutils.UpdatingSchemaFailedState, rs.DbHandle)
		return
	}
	err = warehouseutils.SetUploadStatus(rs.Upload, warehouseutils.UpdatedSchemaState, rs.DbHandle)
	misc.AssertError(err)
	err = warehouseutils.UpdateCurrentSchema(rs.Warehouse, rs.Upload.ID, rs.CurrentSchema, updatedSchema, rs.DbHandle)
	timer.End()
	if err != nil {
		warehouseutils.SetUploadError(rs.Upload, err, warehouseutils.UpdatingSchemaFailedState, rs.DbHandle)
		return
	}
	return
}

func (rs *HandleT) Export() (err error) {
	logger.Debugf("RS: Starting export to redshift for source:%s and wh_upload:%s", rs.Warehouse.Source.ID, rs.Upload.ID)
	err = warehouseutils.SetUploadStatus(rs.Upload, warehouseutils.ExportingDataState, rs.DbHandle)
	misc.AssertError(err)
	timer := warehouseutils.DestStat(stats.TimerType, "upload_time", rs.Warehouse.Destination.ID)
	timer.Start()
	err = rs.load()
	timer.End()
	if err != nil {
		warehouseutils.SetUploadError(rs.Upload, err, warehouseutils.ExportingDataFailedState, rs.DbHandle)
		return err
	}
	err = warehouseutils.SetUploadStatus(rs.Upload, warehouseutils.ExportedDataState, rs.DbHandle)
	misc.AssertError(err)
	return
}

func (rs *HandleT) Process(config warehouseutils.ConfigT) (err error) {
	rs.DbHandle = config.DbHandle
	rs.Warehouse = config.Warehouse
	rs.Upload = config.Upload
	rs.Db, err = connect(RedshiftCredentialsT{
		host:     rs.Warehouse.Destination.Config.(map[string]interface{})["host"].(string),
		port:     rs.Warehouse.Destination.Config.(map[string]interface{})["port"].(string),
		dbName:   rs.Warehouse.Destination.Config.(map[string]interface{})["database"].(string),
		username: rs.Warehouse.Destination.Config.(map[string]interface{})["user"].(string),
		password: rs.Warehouse.Destination.Config.(map[string]interface{})["password"].(string),
	})
	if err != nil {
		warehouseutils.SetUploadError(rs.Upload, err, warehouseutils.UpdatingSchemaFailedState, rs.DbHandle)
		return err
	}
	curreSchema, err := warehouseutils.GetCurrentSchema(rs.DbHandle, rs.Warehouse)
	misc.AssertError(err)
	rs.CurrentSchema = curreSchema.Schema
	rs.Namespace = curreSchema.Namespace
	if rs.Namespace == "" {
		rs.Namespace = rs.Upload.Namespace
	}

	if config.Stage == "ExportData" {
		err = rs.Export()
	} else {
		err = rs.MigrateSchema()
		if err == nil {
			err = rs.Export()
		}
	}
	return
}
