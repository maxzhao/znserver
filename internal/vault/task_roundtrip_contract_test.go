package vault

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestSharedTaskRoundtripContract(t *testing.T) {
	data, err := os.ReadFile("testdata/task-roundtrip.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		SchemaVersion int `json:"schemaVersion"`
		Cases         []struct {
			ID   string `json:"id"`
			Note struct {
				Path   string     `json:"path"`
				Title  string     `json:"title"`
				Folder NoteFolder `json:"folder"`
			} `json:"note"`
			Body              string         `json:"body"`
			ExpectedBody      string         `json:"expectedBody"`
			TaskIndex         int            `json:"taskIndex"`
			ExpectedBefore    map[string]any `json:"expectedBefore"`
			ExpectedAfter     map[string]any `json:"expectedAfter"`
			ExpectedTaskCount int            `json:"expectedTaskCount"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.SchemaVersion != 1 || len(fixture.Cases) == 0 {
		t.Fatal("unsupported or empty task contract fixture")
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.ID, func(t *testing.T) {
			v, err := New(t.TempDir(), Options{})
			if err != nil {
				t.Fatal(err)
			}
			var originalID string
			// The client transforms Markdown; Go stores those exact bytes and
			// parses the resulting task state for the next client read.
			for index, phase := range []struct {
				body string
				want map[string]any
			}{{tc.Body, tc.ExpectedBefore}, {tc.ExpectedBody, tc.ExpectedAfter}} {
				if _, err := v.WriteNote(tc.Note.Path, phase.body); err != nil {
					t.Fatal(err)
				}
				note, err := v.ReadNote(tc.Note.Path)
				if err != nil {
					t.Fatal(err)
				}
				if note.Body != phase.body {
					t.Fatal("storage changed Markdown bytes")
				}
				tasks := ParseTasks(tc.Note.Path, tc.Note.Title, tc.Note.Folder, note.Body)
				if len(tasks) != tc.ExpectedTaskCount || tc.TaskIndex < 0 || tc.TaskIndex >= len(tasks) {
					t.Fatalf("got %d tasks, want %d with index %d", len(tasks), tc.ExpectedTaskCount, tc.TaskIndex)
				}
				task := tasks[tc.TaskIndex]
				if index == 0 {
					originalID = task.ID
				} else if task.ID != originalID {
					t.Fatal("task identity changed after editing")
				}
				encoded, err := json.Marshal(task)
				if err != nil {
					t.Fatal(err)
				}
				var actual map[string]any
				if err := json.Unmarshal(encoded, &actual); err != nil {
					t.Fatal(err)
				}
				for field, want := range phase.want {
					if !reflect.DeepEqual(actual[field], want) {
						t.Errorf("phase %d field %s: got %#v, want %#v", index, field, actual[field], want)
					}
				}
			}
		})
	}
}

func TestSharedTodoSeqContract(t *testing.T) {
	data, err := os.ReadFile("testdata/task-roundtrip.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		TodoSeq []struct {
			ID             string           `json:"id"`
			Body           string           `json:"body"`
			Expected       []map[string]any `json:"expected"`
			ExpectedBody   string           `json:"expectedBody"`
			ExpectedAfter  []map[string]any `json:"expectedAfter"`
			Absent         []string         `json:"absent"`
			NoParents      bool             `json:"noParents"`
			Insert         string           `json:"insert"`
			InsertExpected []map[string]any `json:"insertExpected"`
		} `json:"todoseq"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.TodoSeq) == 0 {
		t.Fatal("empty TODOseq fixture")
	}
	for _, tc := range fixture.TodoSeq {
		t.Run(tc.ID, func(t *testing.T) {
			check := func(body string, want []map[string]any) []Task {
				tasks := ParseTasks("note.md", "Note", FolderInbox, body)
				if len(tasks) != len(want) {
					t.Fatalf("got %d tasks, want %d: %+v", len(tasks), len(want), tasks)
				}
				for i, expected := range want {
					encoded, _ := json.Marshal(tasks[i])
					var actual map[string]any
					_ = json.Unmarshal(encoded, &actual)
					for field, value := range expected {
						got := actual[field]
						if got == nil && value == false {
							got = false
						}
						if !reflect.DeepEqual(got, value) {
							t.Errorf("task %d %s: got %#v want %#v", i, field, got, value)
						}
					}
				}
				return tasks
			}
			before := check(tc.Body, tc.Expected)
			for _, field := range tc.Absent {
				encoded, _ := json.Marshal(before[0])
				var actual map[string]any
				_ = json.Unmarshal(encoded, &actual)
				if _, ok := actual[field]; ok {
					t.Errorf("unexpected %s", field)
				}
			}
			if tc.NoParents {
				for _, task := range before {
					if task.ParentID != "" {
						t.Errorf("unexpected parent: %+v", task)
					}
				}
			}
			if tc.ExpectedBody != "" {
				after := check(tc.ExpectedBody, tc.ExpectedAfter)
				for i := range before {
					if before[i].ID != after[i].ID {
						t.Error("id changed after write")
					}
				}
			}
			if tc.Insert != "" {
				after := check(tc.Insert+tc.Body, tc.InsertExpected)
				var a, b []string
				for _, task := range before {
					if task.TaskIndex >= 0 {
						a = append(a, task.ID)
					}
				}
				for _, task := range after {
					if task.TaskIndex >= 0 {
						b = append(b, task.ID)
					}
				}
				if !reflect.DeepEqual(a, b) {
					t.Error("checkbox ids changed")
				}
			}
		})
	}
}
