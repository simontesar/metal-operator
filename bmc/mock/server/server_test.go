// SPDX-FileCopyrightText: 2025 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

func testdataDir(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine test file path")
	}
	return filepath.Join(filepath.Dir(filename), "testdata", "mock-client", "redfish", "v1")
}

func TestWithDataDirServesDiskData(t *testing.T) {
	srv := NewMockServer(logr.Discard(), ":0", WithDataDir(testdataDir(t)))
	if srv.initErr != nil {
		t.Fatalf("unexpected init error: %v", srv.initErr)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/redfish/v1/", nil)
	srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("service root status = %d, want %d", rec.Code, http.StatusOK)
	}

	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if root["Id"] != "RootService" {
		t.Fatalf("Id = %v, want RootService", root["Id"])
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(
		http.MethodGet,
		"/redfish/v1/Systems/test-system",
		nil,
	)
	srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("system status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestWithDataDirInvalidPathSetsInitErr(t *testing.T) {
	srv := NewMockServer(logr.Discard(), ":0", WithDataDir(t.TempDir()))
	if srv.initErr == nil {
		t.Fatal("expected init error for empty data directory")
	}

	err := srv.Start(context.Background())
	if err == nil {
		t.Fatal("expected Start to return init error")
	}
}

func TestManagerResetUpdatesLastResetTime(t *testing.T) {
	srv := NewMockServer(logr.Discard(), ":0")
	if srv.initErr != nil {
		t.Fatalf("unexpected init error: %v", srv.initErr)
	}

	getManager := func() map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/redfish/v1/Managers/BMC", nil)
		srv.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET manager status = %d, want %d", rec.Code, http.StatusOK)
		}
		body, err := io.ReadAll(rec.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var manager map[string]any
		if err := json.Unmarshal(body, &manager); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		return manager
	}

	before := getManager()
	beforeTime, _ := before["LastResetTime"].(string)
	if beforeTime == "" {
		t.Fatal("expected seeded LastResetTime on Manager")
	}

	beforeReset := time.Now().UTC()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPost,
		"/redfish/v1/Managers/BMC/Actions/Manager.Reset",
		strings.NewReader(`{"ResetType":"GracefulRestart"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	srv.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST reset status = %d, want %d", rec.Code, http.StatusAccepted)
	}

	time.Sleep(300 * time.Millisecond)

	after := getManager()
	afterTime, ok := after["LastResetTime"].(string)
	if !ok || afterTime == "" {
		t.Fatal("expected LastResetTime after BMC reset")
	}
	if afterTime == beforeTime {
		t.Fatalf("LastResetTime unchanged: %s", afterTime)
	}
	parsed, err := time.Parse(time.RFC3339, afterTime)
	if err != nil {
		t.Fatalf("LastResetTime not RFC3339: %v", err)
	}
	if parsed.Before(beforeReset.Add(-time.Second)) {
		t.Fatalf("LastResetTime %v is before reset", parsed)
	}
}
