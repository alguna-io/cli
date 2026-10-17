package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
)

const redacted = "[REDACTED]"

type dryRunOutput struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

// writeDryRun prints the request. The request has no Authorization header yet,
// so the key cannot reach this output.
func writeDryRun(w io.Writer, req *request, hasKey bool) error {
	out := dryRunOutput{Method: req.method, URL: req.url.String(), Headers: map[string]string{}, Body: req.body}
	for name := range req.header {
		out.Headers[name] = req.header.Get(name)
	}
	if hasKey {
		out.Headers["Authorization"] = "Bearer " + redacted
	}
	return encodeJSON(w, out, "  ")
}

// encodeJSON writes v and a newline, without escaping <, > and & (URLs hold &).
func encodeJSON(w io.Writer, v any, indent string) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", indent)
	return encoder.Encode(v)
}

// writeJSON prints a JSON response, indented. With ndjson, a list response prints
// one line per item and any other response prints as one line.
func writeJSON(w io.Writer, format outputFormat, p *Pagination, body []byte) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if !json.Valid(body) {
		_, err := w.Write(body)
		return err
	}
	if format == outputJSON {
		return encodeJSON(w, json.RawMessage(body), "  ")
	}
	if p == nil {
		return writeLines(w, []json.RawMessage{body})
	}
	items, err := pageItems(body, p.Results)
	if err != nil {
		return err
	}
	return writeLines(w, items)
}

func writeLines(w io.Writer, items []json.RawMessage) error {
	for _, item := range items {
		if err := encodeJSON(w, item, ""); err != nil {
			return err
		}
	}
	return nil
}

func writeFile(stdout io.Writer, path string, body []byte) error {
	if path == "" {
		_, err := stdout.Write(body)
		return err
	}
	return os.WriteFile(path, body, 0o644)
}
