package main

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aquasecurity/tracee/types/detect"
	"github.com/aquasecurity/tracee/types/protocol"
	"github.com/aquasecurity/tracee/types/trace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSignature struct {
	detect.Signature
	getMetadata       func() (detect.SignatureMetadata, error)
	getSelectedEvents func() ([]detect.SignatureEventSelector, error)
}

func (f fakeSignature) GetMetadata() (detect.SignatureMetadata, error) {
	if f.getMetadata != nil {
		return f.getMetadata()
	}

	return detect.SignatureMetadata{
		ID:          "FOO-666",
		Name:        "foo bar signature",
		Description: "the most evil",
	}, nil
}

func (f fakeSignature) GetSelectedEvents() ([]detect.SignatureEventSelector, error) {
	if f.getSelectedEvents != nil {
		return f.getSelectedEvents()
	}

	return []detect.SignatureEventSelector{
		{
			Source: "tracee",
			Name:   "execve",
			Origin: "foobar",
		},
		{
			Source: "tracee",
			Name:   "ptrace",
			Origin: "bazfoo",
		},
	}, nil
}

func Test_setupOutput(t *testing.T) {
	var testCases = []struct {
		name           string
		inputEvent     protocol.Event
		outputFormat   string
		expectedOutput string
	}{
		{
			name: "happy path with tracee event and default output",
			inputEvent: trace.Event{
				ProcessName: "foobar.exe",
				HostName:    "foobar.local",
			}.ToProtocol(),
			expectedOutput: `
*** Detection ***
Time: 2021-02-23T01:54:57Z
Signature ID: FOO-666
Signature: foo bar signature
Data: map[foo1:bar1, baz1 foo2:[bar2 baz2]]
Command: foobar.exe
Hostname: foobar.local
`,
		},
		{
			name: "happy path with tracee event and simple custom output template",
			inputEvent: trace.Event{
				ProcessName: "foobar.exe",
				HostName:    "foobar.local",
			}.ToProtocol(),
			expectedOutput: `*** Detection ***
Timestamp: 2021-02-23T01:54:57Z
ProcessName: foobar.exe
HostName: foobar.local
`,
			outputFormat: "templates/simple.tmpl",
		},
		{
			name: "sad path with unknown event",
			inputEvent: protocol.Event{
				Headers: protocol.EventHeaders{
					ContentType: "application/wrong",
					Origin:      "/untrusted",
				},
				Payload: "something wrong",
			},
			expectedOutput: ``,
		},
		{
			name: "sad path with invalid custom template",
			inputEvent: trace.Event{
				ProcessName: "foobar.exe",
				HostName:    "foobar.local",
			}.ToProtocol(),
			outputFormat: "goldens/broken.tmpl",
		},
	}

	for _, tc := range testCases {
		var actualOutput bytes.Buffer
		findingCh, err := setupOutput(&actualOutput, "", "", "", tc.outputFormat)
		require.NoError(t, err, tc.name)

		sm, _ := fakeSignature{}.GetMetadata()
		findingCh <- detect.Finding{
			Data: map[string]interface{}{
				"foo1": "bar1, baz1",
				"foo2": []string{"bar2", "baz2"},
			},
			Event:       tc.inputEvent,
			SigMetadata: sm,
		}

		time.Sleep(time.Millisecond)
		checkOutput(t, tc.name, actualOutput, tc.expectedOutput)
	}
}

func checkOutput(t *testing.T, testName string, actualOutput bytes.Buffer, expectedOutput string) {
	got := strings.Split(actualOutput.String(), "\n")
	for _, g := range got {
		switch {
		case strings.Contains(g, "Time"):
			_, err := time.Parse("2006-01-02T15:04:05Z", strings.Split(g, " ")[1])
			assert.NoError(t, err, testName) // check if time is parsable
		case strings.Contains(g, "time"):
			var gotPayload struct {
				Time time.Time `json:"time"`
			}
			err := json.Unmarshal([]byte(g), &gotPayload)
			assert.NoError(t, err, testName)
			assert.NotEmpty(t, gotPayload, testName) // check if time is parsable
		default:
			assert.Contains(t, expectedOutput, g, testName)
		}
	}
}

func Test_sendToWebhook(t *testing.T) {
	var testCases = []struct {
		name               string
		inputTemplateFile  string
		inputSignature     fakeSignature
		inputTestServerURL string
		contentType        string
		expectedOutput     string
		expectedError      string
	}{
		{
			name:              "happy path with falcosidekick template",
			contentType:       "application/json",
			expectedOutput:    `{"output":"Rule \"foo bar signature\" detection:\n map[foo1:bar1, baz1 foo2:[bar2 baz2]]","rule":"foo bar signature","time":"2021-02-23T01:54:57Z","output_fields":{"value":0}}`,
			inputTemplateFile: "templates/falcosidekick.tmpl",
		},
		{
			name:        "happy path, with simple template",
			contentType: "text/plain",
			expectedOutput: `*** Detection ***
Timestamp: 2021-02-23T01:54:57Z
ProcessName: foobar.exe
HostName: foobar.local
`,
			inputTemplateFile: "templates/simple.tmpl",
		},
		{
			name:        "happy path with functions from sprig template",
			contentType: "text/plain",
			expectedOutput: `{
  "foo1": "bar1, baz1",
  "foo2": [
    "bar2",
    "baz2"
  ]
}`,
			inputTemplateFile: "templates/sprig.tmpl",
		},
		{
			name:               "sad path, error reaching webhook",
			inputTestServerURL: "foo://bad.host",
			expectedError:      `error calling webhook Post "foo://bad.host": unsupported protocol scheme "foo"`,
			inputTemplateFile:  "templates/simple.tmpl",
		},
		{
			name:              "sad path, with missing template",
			inputTemplateFile: "invalid/template",
			expectedError:     `error writing to template: template not initialized`,
		},
		{
			name:              "sad path, with an invalid template",
			contentType:       "application/foo",
			inputTemplateFile: "goldens/broken.tmpl",
			expectedError:     `error writing to the template: template: broken.tmpl:1:3: executing "broken.tmpl" at <.InvalidField>: can't evaluate field InvalidField in type detect.Finding`,
		},
		{
			name:          "sad path, no --webhook-template flag specified",
			expectedError: `error sending to webhook: --webhook-template flag is required when using --webhook flag`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				got, _ := ioutil.ReadAll(request.Body)
				checkOutput(t, tc.name, *bytes.NewBuffer(got), tc.expectedOutput)
				assert.Equal(t, tc.contentType, request.Header.Get("content-type"), tc.name)
			}))
			defer ts.Close()

			if tc.inputTestServerURL != "" {
				ts.URL = tc.inputTestServerURL
			}

			inputTemplate, _ := setupTemplate(tc.inputTemplateFile)

			m, _ := tc.inputSignature.GetMetadata()
			actualError := sendToWebhook(inputTemplate, detect.Finding{
				Data: map[string]interface{}{
					"foo1": "bar1, baz1",
					"foo2": []string{"bar2", "baz2"},
				},
				Event: trace.Event{
					ProcessName: "foobar.exe",
					HostName:    "foobar.local",
				}.ToProtocol(),
				SigMetadata: m,
			}, ts.URL, tc.inputTemplateFile, tc.contentType)

			switch {
			case tc.expectedError != "":
				assert.EqualError(t, actualError, tc.expectedError, tc.name)
			default:
				assert.NoError(t, actualError, tc.name)
			}
		})

	}
}

func TestOutputTemplates(t *testing.T) {
	testCases := []struct {
		testName     string
		finding      detect.Finding
		expectedJson string
	}{
		{
			testName: "Should output finding as raw JSON",
			finding: detect.Finding{
				Data: map[string]interface{}{
					"a": 123,
					"b": "c",
					"d": true,
					"f": map[string]string{
						"123": "456",
						"foo": "bar",
					},
				},
				Event: trace.Event{
					ProcessID:   21312,
					Timestamp:   1321321,
					UserID:      0,
					ContainerID: "abbc123",
					EventName:   "execve",
				}.ToProtocol(),
				SigMetadata: detect.SignatureMetadata{
					ID:          "TRC-1",
					Version:     "0.1.0",
					Name:        "Standard Input/Output Over Socket",
					Description: "Redirection of process's standard input/output to socket",
					Tags:        []string{"linux", "container"},
					Properties: map[string]interface{}{
						"Severity":     3,
						"MITRE ATT&CK": "Persistence: Server Software Component",
					},
				},
			},
			expectedJson: `{
				"Data": {
					"a":123,"b":"c","d":true,"f":{"123":"456","foo":"bar"}
				},
				"Context":{
					"timestamp":1321321,"processorId":0,"processId":21312,"threadId":0,"parentProcessId":0,"hostProcessId":0,"hostThreadId":0,"hostParentProcessId":0,"userId":0,"mountNamespace":0,"pidNamespace":0,"processName":"","hostName":"","containerId":"abbc123","eventId":"0","eventName":"execve","argsNum":0,"returnValue":0,"stackAddresses":null,"args":null
				},
				"SigMetadata":{
					"ID":"TRC-1","Version":"0.1.0","Name":"Standard Input/Output Over Socket","Description":"Redirection of process's standard input/output to socket","Tags":["linux","container"],"Properties":{"MITRE ATT\u0026CK":"Persistence: Server Software Component","Severity":3}
				}
			}`,
		},
	}

	jsonTemplate, err := setupTemplate("templates/rawjson.tmpl")
	require.NoError(t, err)

	for _, tc := range testCases {
		t.Run(tc.testName, func(t *testing.T) {
			var buf bytes.Buffer
			err := jsonTemplate.Execute(&buf, tc.finding)
			require.NoError(t, err)
			assert.JSONEq(t, tc.expectedJson, buf.String())
		})
	}

}
