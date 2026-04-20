package executor

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/assast/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/assast/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/assast/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/assast/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexExecutorExecutePreservesPreviousResponseID(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}
	payload := []byte(`{"model":"gpt-5.4","previous_response_id":"resp-prev","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"follow up"}]}]}`)

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/responses" {
		t.Fatalf("path = %q, want %q", gotPath, "/responses")
	}
	if got := gjson.GetBytes(gotBody, "previous_response_id").String(); got != "resp-prev" {
		t.Fatalf("previous_response_id = %q, want %q in upstream body %s", got, "resp-prev", gotBody)
	}
	if gjson.GetBytes(gotBody, "stream_options").Exists() {
		t.Fatalf("stream_options leaked upstream: %s", gotBody)
	}
	if gjson.GetBytes(resp.Payload, "id").String() != "resp_1" {
		t.Fatalf("response id = %q, want %q", gjson.GetBytes(resp.Payload, "id").String(), "resp_1")
	}
}

func TestCodexExecutorExecuteStreamPreservesPreviousResponseID(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_2\",\"output\":[]}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}
	payload := []byte(`{"model":"gpt-5.4","previous_response_id":"resp-prev","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"follow up"}]}],"stream":true}`)

	stream, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	if gotPath != "/responses" {
		t.Fatalf("path = %q, want %q", gotPath, "/responses")
	}
	if got := gjson.GetBytes(gotBody, "previous_response_id").String(); got != "resp-prev" {
		t.Fatalf("previous_response_id = %q, want %q in upstream body %s", got, "resp-prev", gotBody)
	}
	if gjson.GetBytes(gotBody, "stream_options").Exists() {
		t.Fatalf("stream_options leaked upstream: %s", gotBody)
	}

	var lines []string
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		scanner := bufio.NewScanner(strings.NewReader(string(chunk.Payload)))
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, `"type":"response.completed"`) {
		t.Fatalf("expected completed payload in stream, got %q", joined)
	}
}
