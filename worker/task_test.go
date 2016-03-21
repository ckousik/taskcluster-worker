package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/taskcluster/slugid-go/slugid"
	"github.com/taskcluster/taskcluster-client-go/queue"
	"github.com/taskcluster/taskcluster-client-go/tcclient"
	"github.com/taskcluster/taskcluster-worker/config"
	"github.com/taskcluster/taskcluster-worker/engines"
	"github.com/taskcluster/taskcluster-worker/engines/extpoints"
	"github.com/taskcluster/taskcluster-worker/plugins"
	pluginExtpoints "github.com/taskcluster/taskcluster-worker/plugins/extpoints"
	"github.com/taskcluster/taskcluster-worker/runtime"
)

var logger, _ = runtime.CreateLogger(os.Getenv("LOGGING_LEVEL"))

var taskDefinitions = map[string]struct {
	definition string
	success    bool
}{
	"invalidJSON": {
		definition: "",
		success:    false,
	},
	"invalidEnginePayload": {
		definition: `{"start": {"delay1": 10,"function": "write-log","argument": "Hello World"}}`,
		success:    false,
	},
	"validEnginePayload": {
		definition: `{"start": {"delay": 10,"function": "write-log","argument": "Hello World"}}`,
		success:    true,
	},
}

var claim = &taskClaim{
	taskID: "abc",
	runID:  1,
	taskClaim: &queue.TaskClaimResponse{
		Credentials: struct {
			AccessToken string `json:"accessToken"`
			Certificate string `json:"certificate"`
			ClientID    string `json:"clientId"`
		}{
			AccessToken: "123",
			ClientID:    "abc",
			Certificate: "",
		},
	},
	definition: &queue.TaskDefinitionResponse{
		Payload: []byte(taskDefinitions["validEnginePayload"].definition),
	},
}

type mockedPluginManager struct {
	payloadSchema      runtime.CompositeSchema
	payloadSchemaError error
	taskPlugin         plugins.TaskPlugin
	taskPluginError    error
}

func (m mockedPluginManager) PayloadSchema() (runtime.CompositeSchema, error) {
	return m.payloadSchema, m.payloadSchemaError
}

func (m mockedPluginManager) NewTaskPlugin(options plugins.TaskPluginOptions) (plugins.TaskPlugin, error) {
	return m.taskPlugin, m.taskPluginError
}

type plugin struct {
	plugins.PluginBase
}

type taskPlugin struct {
	plugins.TaskPluginBase
}

func ensureEnvironment(t *testing.T) (*runtime.Environment, engines.Engine, plugins.Plugin) {
	tempPath := filepath.Join(os.TempDir(), slugid.V4())
	tempStorage, err := runtime.NewTemporaryStorage(tempPath)
	if err != nil {
		t.Fatal(err)
	}

	environment := &runtime.Environment{
		TemporaryStorage: tempStorage,
	}
	engineProvider := extpoints.EngineProviders.Lookup("mock")
	engine, err := engineProvider.NewEngine(extpoints.EngineOptions{
		Environment: environment,
		Log:         logger.WithField("engine", "mock"),
	})
	if err != nil {
		t.Fatal(err.Error())
	}

	pluginOptions := &pluginExtpoints.PluginOptions{
		Environment: environment,
		Engine:      &engine,
		Log:         logger.WithField("component", "Plugin Manager"),
	}

	pm, err := pluginExtpoints.NewPluginManager([]string{"success"}, *pluginOptions)
	if err != nil {
		t.Fatalf("Error creating task manager. Could not create plugin manager. %s", err)
	}

	return environment, engine, pm
}

func TestParsePayload(t *testing.T) {
	var err error
	environment, engine, pluginManager := ensureEnvironment(t)

	tr := &TaskRun{
		TaskID:        "abc",
		RunID:         1,
		log:           logger.WithField("taskId", "abc"),
		pluginManager: pluginManager,
		engine:        engine,
	}

	tp := environment.TemporaryStorage.NewFilePath()
	tr.context, tr.controller, err = runtime.NewTaskContext(tp, claim.taskClaim)
	defer func() {
		tr.controller.CloseLog()
		tr.controller.Dispose()
	}()

	for name, tc := range taskDefinitions {
		tr.definition = &queue.TaskDefinitionResponse{
			Payload: []byte(tc.definition),
		}
		err = tr.parsePayload()
		assert.Equal(
			t,
			tc.success, err == nil,
			fmt.Sprintf("Parsing task payload '%s' did not result in expected outcome.", name),
		)
	}
}

func TestCreateTaskPlugins(t *testing.T) {
	var err error
	environment, engine, pluginManager := ensureEnvironment(t)
	tr, err := NewTaskRun(&config.Config{}, claim, environment, engine, pluginManager, logger.WithField("test", "TestRunTask"))
	assert.Nil(t, err)

	err = tr.parsePayload()
	if err != nil {
		t.Fatal(err)
	}

	tr.pluginManager = mockedPluginManager{
		taskPlugin: &taskPlugin{},
	}

	err = tr.createTaskPlugins()
	assert.Nil(t, err, "Error should not have been returned when creating task plugins")

	tr.pluginManager = mockedPluginManager{
		taskPlugin:      nil,
		taskPluginError: engines.NewMalformedPayloadError("bad payload"),
	}

	err = tr.createTaskPlugins()
	assert.NotNil(t, err, "Error should have been returned when creating task plugins")
	assert.Equal(t, "engines.MalformedPayloadError", reflect.TypeOf(err).String())
}

func TestRunTask(t *testing.T) {
	environment, engine, pluginManager := ensureEnvironment(t)
	tr, err := NewTaskRun(&config.Config{}, claim, environment, engine, pluginManager, logger.WithField("test", "TestRunTask"))
	assert.Nil(t, err)

	mockedQueue := &runtime.MockQueue{}
	mockedQueue.On(
		"ReportCompleted",
		"abc",
		"1",
	).Return(&queue.TaskStatusResponse{}, &tcclient.CallSummary{}, nil)

	tr.controller.SetQueueClient(mockedQueue)

	tr.Run()
	mockedQueue.AssertCalled(t, "ReportCompleted", "abc", "1")
}

func TestRunMalformedEnginePayloadTask(t *testing.T) {
	claim.definition = &queue.TaskDefinitionResponse{
		Payload: []byte(taskDefinitions["invalidEnginePayload"].definition),
	}

	environment, engine, pluginManager := ensureEnvironment(t)
	tr, err := NewTaskRun(&config.Config{}, claim, environment, engine, pluginManager, logger.WithField("test", "TestRunTask"))
	assert.Nil(t, err)

	mockedQueue := &runtime.MockQueue{}
	mockedQueue.On(
		"ReportException",
		"abc",
		"1",
		&queue.TaskExceptionRequest{Reason: "malformed-payload"},
	).Return(&queue.TaskStatusResponse{}, &tcclient.CallSummary{}, nil)

	tr.controller.SetQueueClient(mockedQueue)

	tr.Run()
	mockedQueue.AssertCalled(t, "ReportException", "abc", "1", &queue.TaskExceptionRequest{Reason: "malformed-payload"})
}
