package backendconfig

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"reflect"
	"time"

	"github.com/rudderlabs/rudder-server/services/stats"

	"github.com/rudderlabs/rudder-server/utils/logger"

	"github.com/rudderlabs/rudder-server/config"
	"github.com/rudderlabs/rudder-server/utils"
)

var (
	configBackendURL, configBackendToken string
	pollInterval                         time.Duration
	curSourceJSON                        SourcesT
	initialized                          bool
)

var Eb *utils.EventBus

type DestinationDefinitionT struct {
	ID          string
	Name        string
	DisplayName string
}

type SourceDefinitionT struct {
	ID   string
	Name string
}

type DestinationT struct {
	ID                    string
	Name                  string
	DestinationDefinition DestinationDefinitionT
	Config                interface{}
	Enabled               bool
	Transformations       []TransformationT
}

type SourceT struct {
	ID               string
	Name             string
	SourceDefinition SourceDefinitionT
	Config           interface{}
	Enabled          bool
	Destinations     []DestinationT
	WriteKey         string
}

type SourcesT struct {
	Sources []SourceT `json:"sources"`
}

type TransformationT struct {
	ID          string
	Name        string
	Description string
	VersionID   string
}

func loadConfig() {
	configBackendURL = config.GetEnv("CONFIG_BACKEND_URL", "https://api.rudderlabs.com")
	configBackendToken = config.GetEnv("CONFIG_BACKEND_TOKEN", "1P2tfQQKarhlsG6S3JGLdXptyZY")
	pollInterval = config.GetDuration("BackendConfig.pollIntervalInS", 5) * time.Second
}

func getBackendConfig() (SourcesT, bool) {
	client := &http.Client{}
	url := fmt.Sprintf("%s/workspace-config?workspaceToken=%s", configBackendURL, configBackendToken)
	resp, err := client.Get(url)

	var respBody []byte
	if resp != nil && resp.Body != nil {
		respBody, _ = ioutil.ReadAll(resp.Body)
		defer resp.Body.Close()
	}
	if err != nil {
		logger.Error("Errored when sending request to the server", err)
		return SourcesT{}, false
	}
	var sourcesJSON SourcesT
	err = json.Unmarshal(respBody, &sourcesJSON)
	if err != nil {
		logger.Error("Errored while parsing request", err, string(respBody), resp.StatusCode)
		return SourcesT{}, false
	}
	return sourcesJSON, true
}

func init() {
	config.Initialize()
	loadConfig()
}

func pollConfigUpdate() {
	statConfigBackendError := stats.NewStat("config_backend.errors", stats.CountType)
	for {
		sourceJSON, ok := getBackendConfig()
		if !ok {
			statConfigBackendError.Increment()
		}
		if ok && !reflect.DeepEqual(curSourceJSON, sourceJSON) {
			curSourceJSON = sourceJSON
			initialized = true
			Eb.Publish("backendconfig", sourceJSON)
		}
		time.Sleep(time.Duration(pollInterval))
	}
}

func GetConfig() SourcesT {
	return curSourceJSON
}

func Subscribe(channel chan utils.DataEvent) {
	Eb.Subscribe("backendconfig", channel)
	Eb.PublishToChannel(channel, "backendconfig", curSourceJSON)
}

func WaitForConfig() {
	for {
		if initialized {
			break
		}
		logger.Info("Waiting for initializing backend config")
		time.Sleep(time.Duration(pollInterval))

	}
}

// Setup backend config
func Setup() {
	Eb = new(utils.EventBus)
	go pollConfigUpdate()
}
