package httpserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func activityRequest(t *testing.T, url, method, body, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, strings.TrimSpace(string(raw))
}

func TestActivityOrderPreservesMetadata(t *testing.T) {
	const order = `["beta/main","alpha/main"]`
	cases := []struct{ name, input, want string }{
		{"flow", "# project\nname: 'Keep  quoting' # name\nzennotes: # server\n  port: 7878 # port\n  activity_order: [old/main] # order\n  other: {x: yes}\ntail: |\n  content\n", "# project\nname: 'Keep  quoting' # name\nzennotes: # server\n  port: 7878 # port\n  activity_order: " + order + " # order\n  other: {x: yes}\ntail: |\n  content\n"},
		{"block", "zennotes:\n  port: 7878\n  activity_order:\n    - old/main\n    - older/main # keep this comment\n  next: 9\n", "zennotes:\n  port: 7878\n  activity_order:\n    " + order + " # keep this comment\n  next: 9\n"},
		{"missing-key", "# header\nzennotes:\n  # port comment\n  port: 7878 # stay\nother: yes\n", "# header\nzennotes:\n  # port comment\n  activity_order: " + order + "\n  port: 7878 # stay\nother: yes\n"},
		{"missing-mapping", "# header\nname: 'keep'\nother: yes\n", "# header\nzennotes:\n  activity_order: " + order + "\nname: 'keep'\nother: yes\n"},
		{"flow-mapping", "name: yes\nzennotes: {port: 7878} # keep\n", "name: yes\nzennotes: {port: 7878, activity_order: " + order + "} # keep\n"},
		{"empty-flow-mapping", "zennotes: {} # keep\n", "zennotes: {activity_order: " + order + "} # keep\n"},
		{"root-flow", "{name: 'keep'} # tail\n", "{name: 'keep', zennotes: {activity_order: " + order + "}} # tail\n"},
		{"empty", "", "zennotes:\n  activity_order: " + order + "\n"},
		{"comments-only", "# keep\n", "# keep\nzennotes:\n  activity_order: " + order + "\n"},
		{"crlf", "name: keep\r\nzennotes:\r\n  port: 7878\r\n", "name: keep\r\nzennotes:\r\n  activity_order: " + order + "\r\n  port: 7878\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, ts, _, _ := extServer(t, "token", "/base")
			path := filepath.Join(s.currentVault().Root(), "_meta.yaml")
			extWrite(t, path, tc.input)
			if err := os.Chmod(path, 0640); err != nil {
				t.Fatal(err)
			}
			url := ts.URL + "/base/api/maxa/activity-order"
			status, body := activityRequest(t, url, "PUT", `{"order":`+order+`}`, "token")
			if status != 200 || body != `{"order":`+order+`}` {
				t.Fatalf("PUT %d %s", status, body)
			}
			raw, err := os.ReadFile(path)
			if err != nil || string(raw) != tc.want {
				t.Fatalf("metadata changed unexpectedly: %v\ngot %q\nwant %q", err, raw, tc.want)
			}
			info, _ := os.Stat(path)
			if info.Mode().Perm() != 0640 {
				t.Fatalf("mode %o", info.Mode().Perm())
			}
			status, body = activityRequest(t, url, "GET", "", "token")
			if status != 200 || body != `{"order":`+order+`}` {
				t.Fatalf("GET %d %s", status, body)
			}
			if !s.currentVault().IsExcluded("_meta.yaml") {
				t.Fatal("metadata visible as an asset")
			}
		})
	}
}

func TestActivityOrderMissingValidationAndAuth(t *testing.T) {
	s, ts, _, _ := extServer(t, "token", "/zn")
	path := filepath.Join(s.currentVault().Root(), "_meta.yaml")
	url := ts.URL + "/zn/api/maxa/activity-order"
	if status, body := activityRequest(t, url, "GET", "", "token"); status != 200 || body != `{"order":[]}` {
		t.Fatal(status, body)
	}
	if status, _ := activityRequest(t, url, "PUT", `{"order":[]}`, "token"); status != 409 {
		t.Fatal(status)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("PUT created missing metadata", err)
	}
	for _, method := range []string{"GET", "PUT"} {
		if status, _ := activityRequest(t, url, method, `{"order":[]}`, ""); status != 401 {
			t.Fatal(method, status)
		}
	}
	const original = "name: 'keep' # comment\nzennotes:\n  port: 7878\n"
	extWrite(t, path, original)
	if status, body := activityRequest(t, url, "GET", "", "token"); status != 200 || body != `{"order":[]}` {
		t.Fatal(status, body)
	}
	tooMany := make([]string, 257)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("demo/view-%d", i)
	}
	large, _ := json.Marshal(activityOrderResponse{Order: tooMany})
	for _, body := range []string{`{}`, `{"order":null}`, `{"order":{}}`, `{"order":[null]}`, `{"order":[5]}`, `{"order":["bad"]}`, `{"order":["a/b/c"]}`, `{"order":["UPPER/main"]}`, `{"order":["a/main","a/main"]}`, `{"order":["../main"]}`, `{"order":["a/` + strings.Repeat("x", 65) + `"]}`, `{"order":[],"extra":1}`, `{"order":[]} {}`, string(large)} {
		if status, response := activityRequest(t, url, "PUT", body, "token"); status != 400 {
			t.Fatalf("accepted %s: %d %s", body, status, response)
		}
		raw, _ := os.ReadFile(path)
		if string(raw) != original {
			t.Fatal("invalid PUT modified metadata")
		}
	}
	if status, body := activityRequest(t, url, "PUT", `{"order":[]}`, "token"); status != 200 || body != `{"order":[]}` {
		t.Fatal(status, body)
	}
	for _, malformed := range []string{"zennotes: wrong\n", "zennotes:\n  activity_order: &shared [a/main]\ncopy: *shared\n", "zennotes: &shared {activity_order: [a/main]}\ncopy: *shared\n", "defaults: &defaults {zennotes: {port: 7878}}\n<<: *defaults\n", "zennotes: {}\nzennotes: {}\n"} {
		extWrite(t, path, malformed)
		if status, _ := activityRequest(t, url, "PUT", `{"order":[]}`, "token"); status != 409 {
			t.Fatal(status)
		}
		raw, _ := os.ReadFile(path)
		if string(raw) != malformed {
			t.Fatal("unsafe structure modified")
		}
	}
}

func TestExtensionsOrderManifestAndListing(t *testing.T) {
	_, ts, project, _ := extServer(t, "", "")
	pkg := extPackage(t, project, "demo", "Demo", "")
	path := filepath.Join(pkg, "web/extension.json")
	original, _ := os.ReadFile(path)
	for _, value := range []string{"0", "5", "10000", "-1", "10001", "1.5", "\"5\"", "true", "null", "{}"} {
		body := strings.TrimSuffix(string(original), "}") + `,"order":` + value + `}`
		extWrite(t, path, body)
		e := extList(t, ts.URL+"/api/ext")[0]
		valid := value == "0" || value == "5" || value == "10000"
		if valid != (e.Error == nil) {
			t.Fatalf("order=%s: %+v", value, e)
		}
		raw, _ := json.Marshal(e)
		var fields map[string]json.RawMessage
		json.Unmarshal(raw, &fields)
		if valid && string(fields["order"]) != value {
			t.Fatalf("order omitted/wrong: %s", raw)
		}
		if !valid && len(fields) != 3 {
			t.Fatalf("invalid shape: %s", raw)
		}
	}
	extWrite(t, path, string(original))
	raw, _ := json.Marshal(extList(t, ts.URL+"/api/ext")[0])
	var fields map[string]json.RawMessage
	json.Unmarshal(raw, &fields)
	if _, ok := fields["order"]; ok {
		t.Fatal("absent order was emitted", string(raw))
	}
}
