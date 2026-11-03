package httpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

type activityOrderResponse struct {
	Order []string `json:"order"`
}

func validActivityOrder(order []string) bool {
	if order == nil || len(order) > 256 {
		return false
	}
	seen := map[string]bool{}
	for _, id := range order {
		parts := strings.Split(id, "/")
		if len(parts) != 2 || !validExtensionName(parts[0]) || !validExtensionName(parts[1]) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func (s *Server) activityOrder(w http.ResponseWriter, r *http.Request) {
	var requested activityOrderResponse
	if r.Method == http.MethodPut {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var extra any
		if err := dec.Decode(&requested); err != nil || dec.Decode(&extra) != io.EOF || !validActivityOrder(requested.Order) {
			http.Error(w, "order must be at most 256 unique extension/view identifiers", http.StatusBadRequest)
			return
		}
	}
	// Serialize Web read/modify/write requests, including clients of the same project.
	s.activityOrderMu.Lock()
	defer s.activityOrderMu.Unlock()
	root, err := os.OpenRoot(s.currentVault().Root())
	if err != nil {
		http.Error(w, "vault unavailable", 500)
		return
	}
	defer root.Close()
	info, err := root.Lstat("_meta.yaml")
	if errors.Is(err, os.ErrNotExist) {
		if r.Method == http.MethodGet {
			writeJSON(w, 200, activityOrderResponse{Order: []string{}})
		} else {
			http.Error(w, "_meta.yaml is missing", http.StatusConflict)
		}
		return
	}
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "_meta.yaml must be a regular project file", http.StatusConflict)
		return
	}
	raw, err := root.ReadFile("_meta.yaml")
	if err != nil {
		http.Error(w, "cannot read _meta.yaml", 500)
		return
	}
	doc, err := activityMetadata(raw)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	zen, err := metadataValue(doc, "zennotes")
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	orderNode, err := metadataValue(zen, "activity_order")
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if r.Method == http.MethodGet {
		order := []string{}
		if orderNode != nil {
			if err := orderNode.Decode(&order); err != nil || !validActivityOrder(order) {
				http.Error(w, "invalid stored activity_order", http.StatusConflict)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		writeJSON(w, 200, activityOrderResponse{Order: order})
		return
	}
	next, err := patchActivityOrder(raw, doc, zen, orderNode, requested.Order)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	// Verify the splice with the YAML parser before publishing any bytes.
	parsed, err := activityMetadata(next)
	if err == nil {
		var z, o *yaml.Node
		z, err = metadataValue(parsed, "zennotes")
		if err == nil {
			o, err = metadataValue(z, "activity_order")
		}
		var stored []string
		if err == nil && o != nil {
			err = o.Decode(&stored)
		} else if err == nil {
			err = errors.New("missing stored order")
		}
		if err == nil && !equalActivityOrder(stored, requested.Order) {
			err = errors.New("stored order differs")
		}
	}
	if err != nil {
		http.Error(w, "cannot safely update activity_order", http.StatusConflict)
		return
	}
	// Refuse an observed external edit instead of overwriting another metadata writer.
	current, err := root.ReadFile("_meta.yaml")
	if err != nil || !bytes.Equal(current, raw) {
		http.Error(w, "_meta.yaml changed; retry", http.StatusConflict)
		return
	}
	name := fmt.Sprintf("_meta.yaml.%d.%d.tmp", os.Getpid(), time.Now().UnixNano())
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		defer root.Remove(name)
		err = f.Chmod(info.Mode().Perm())
		if err == nil {
			_, err = f.Write(next)
		}
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			err = root.Rename(name, "_meta.yaml")
		}
	}
	if err != nil {
		http.Error(w, "cannot atomically save activity_order", 500)
		return
	}
	writeJSON(w, 200, requested)
}

func equalActivityOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func activityMetadata(raw []byte) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var doc, extra yaml.Node
	err := dec.Decode(&doc)
	if err == io.EOF {
		return nil, nil
	}
	if err != nil || dec.Decode(&extra) != io.EOF {
		return nil, errors.New("_meta.yaml must contain one YAML document")
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	node := doc.Content[0]
	if node.Tag == "!!null" {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, errors.New("_meta.yaml must contain a mapping")
	}
	return node, nil
}

func metadataValue(mapping *yaml.Node, key string) (*yaml.Node, error) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil, nil
	}
	var value *yaml.Node
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != key {
			continue
		}
		if value != nil {
			return nil, fmt.Errorf("duplicate metadata key %s", key)
		}
		value = mapping.Content[i+1]
	}
	return value, nil
}

// Node coordinates locate the one editable value. Re-encoding the document with
// yaml.Encoder would preserve meaning but change unrelated quoting/spacing/comments.
func metadataOffset(raw []byte, node *yaml.Node) int {
	pos := 0
	for line := 1; line < node.Line; line++ {
		pos += bytes.IndexByte(raw[pos:], '\n') + 1
	}
	for col := 1; col < node.Column; col++ {
		_, size := utf8.DecodeRune(raw[pos:])
		pos += size
	}
	return pos
}

// End of a flow collection or a single-line scalar, excluding trailing comments.
func metadataTokenEnd(raw []byte, start int) (int, error) {
	var quote byte
	depth := 0
	for i := start; i < len(raw); i++ {
		c := raw[i]
		if quote != 0 {
			if quote == '"' && c == '\\' {
				i++
				continue
			}
			if c == quote {
				if quote == '\'' && i+1 < len(raw) && raw[i+1] == '\'' {
					i++
					continue
				}
				quote = 0
				if depth == 0 {
					return i + 1, nil
				}
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c == '#' {
			if depth == 0 {
				return i, nil
			}
			for i < len(raw) && raw[i] != '\n' {
				i++
			}
			continue
		}
		if c == '[' || c == '{' {
			depth++
			continue
		}
		if c == ']' || c == '}' {
			if depth == 0 {
				return i, nil
			}
			depth--
			if depth == 0 {
				return i + 1, nil
			}
		}
		if depth == 0 && (c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == ',') {
			return i, nil
		}
	}
	if depth != 0 || quote != 0 {
		return 0, errors.New("unterminated metadata value")
	}
	return len(raw), nil
}

func patchActivityOrder(raw []byte, doc, zen, order *yaml.Node, ids []string) ([]byte, error) {
	if (doc != nil && doc.Anchor != "") || (zen != nil && (zen.Anchor != "" || zen.Kind == yaml.AliasNode)) {
		return nil, errors.New("anchored metadata mappings cannot be updated safely")
	}
	if zen == nil && doc != nil {
		for i := 0; i < len(doc.Content); i += 2 {
			if doc.Content[i].Tag == "!!merge" {
				return nil, errors.New("cannot shadow a merged zennotes mapping")
			}
		}
	}
	value, _ := json.Marshal(ids) // JSON arrays are YAML flow sequences; identifiers need no escaping.
	newline := "\n"
	if bytes.Contains(raw, []byte("\r\n")) {
		newline = "\r\n"
	}
	splice := func(start, end int, text string) []byte {
		out := append([]byte{}, raw[:start]...)
		out = append(out, text...)
		return append(out, raw[end:]...)
	}
	if order != nil {
		if order.Kind != yaml.SequenceNode || order.Anchor != "" {
			return nil, errors.New("activity_order must be an unanchored sequence")
		}
		var existing []string
		if err := order.Decode(&existing); err != nil || !validActivityOrder(existing) {
			return nil, errors.New("invalid stored activity_order")
		}
		start := metadataOffset(raw, order)
		last := order
		if order.Style&yaml.FlowStyle == 0 && len(order.Content) > 0 {
			last = order.Content[len(order.Content)-1]
		}
		end, err := metadataTokenEnd(raw, metadataOffset(raw, last))
		if err != nil {
			return nil, err
		}
		// Keep comments inside the replaced sequence as standalone comments at the
		// same value indentation. Comments outside the value span remain byte-identical.
		var comments strings.Builder
		for _, line := range bytes.Split(raw[start:end], []byte("\n")) {
			if at := bytes.IndexByte(line, '#'); at >= 0 {
				comments.Write(bytes.TrimSuffix(line[at:], []byte("\r")))
				comments.WriteString(newline)
				comments.WriteString(strings.Repeat(" ", order.Column-1))
			}
		}
		return splice(start, end, comments.String()+string(value)), nil
	}
	if zen != nil {
		if zen.Kind != yaml.MappingNode || zen.Anchor != "" {
			return nil, errors.New("zennotes must be an unanchored mapping")
		}
		if zen.Style&yaml.FlowStyle != 0 {
			end, err := metadataTokenEnd(raw, metadataOffset(raw, zen))
			if err != nil {
				return nil, err
			}
			prefix := ""
			if len(zen.Content) > 0 {
				prefix = ", "
			}
			return splice(end-1, end-1, prefix+"activity_order: "+string(value)), nil
		}
		first := zen.Content[0]
		pos := metadataOffset(raw, first)
		line := bytes.LastIndexByte(raw[:pos], '\n') + 1
		return splice(line, line, strings.Repeat(" ", first.Column-1)+"activity_order: "+string(value)+newline), nil
	}
	if doc != nil && doc.Style&yaml.FlowStyle != 0 {
		end, err := metadataTokenEnd(raw, metadataOffset(raw, doc))
		if err != nil {
			return nil, err
		}
		prefix := ""
		if len(doc.Content) > 0 {
			prefix = ", "
		}
		return splice(end-1, end-1, prefix+"zennotes: {activity_order: "+string(value)+"}"), nil
	}
	// Insert at the beginning of the root mapping (after any document header),
	// or append to an empty/comment-only file. Never rewrite another key.
	pos := len(raw)
	if doc != nil && len(doc.Content) > 0 {
		pos = metadataOffset(raw, doc.Content[0])
		pos = bytes.LastIndexByte(raw[:pos], '\n') + 1
	}
	prefix := ""
	if pos > 0 && raw[pos-1] != '\n' {
		prefix = newline
	}
	return splice(pos, pos, prefix+"zennotes:"+newline+"  activity_order: "+string(value)+newline), nil
}
