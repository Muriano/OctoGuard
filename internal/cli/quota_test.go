package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Muriano/OctoGuard/internal/auth"
	"github.com/Muriano/OctoGuard/internal/quota"
	"github.com/Muriano/OctoGuard/testdata/fakegh"
)

const rateLimitBody = `{"resources":{
	"core":{"limit":5000,"remaining":4000,"reset":1900000000,"used":1000},
	"graphql":{"limit":5000,"remaining":4500,"reset":1900000000,"used":500},
	"search":{"limit":30,"remaining":30,"reset":1900000000,"used":0}}}`

type quotaOutput struct {
	SchemaVersion int `json:"schema_version"`
	Core          struct {
		Remaining int  `json:"remaining"`
		Limit     int  `json:"limit"`
		Known     bool `json:"known"`
	} `json:"core"`
	GraphQL struct {
		Remaining int  `json:"remaining"`
		Known     bool `json:"known"`
	} `json:"graphql"`
	Search struct {
		Remaining int  `json:"remaining"`
		Known     bool `json:"known"`
	} `json:"search"`
	Meta struct {
		Probed bool `json:"probed"`
	} `json:"meta"`
}

func TestQuota_NoAPICallWhenKnown(t *testing.T) {
	srv := fakegh.New(t)
	srv.SetResource("/rate_limit", []byte(rateLimitBody), "")
	reset := time.Now().Add(time.Hour)
	snap := map[string]quota.Bucket{
		"core":    {Remaining: 10, Limit: 5000, Reset: reset, Known: true},
		"graphql": {Remaining: 20, Limit: 5000, Reset: reset, Known: true},
		"search":  {Remaining: 30, Limit: 30, Reset: reset, Known: true},
	}
	buckets, probed, err := completeQuota(context.Background(), srv.URL, auth.FromValue("t"), snap)
	if err != nil {
		t.Fatal(err)
	}
	if probed {
		t.Fatal("probed with every bucket known")
	}
	if n := srv.Calls("/rate_limit"); n != 0 {
		t.Fatalf("/rate_limit calls = %d, want 0", n)
	}
	if buckets["core"].Remaining != 10 || buckets["search"].Remaining != 30 {
		t.Fatalf("buckets changed: %+v", buckets)
	}
}

func TestQuota_ProbesOnceWhenUnknown(t *testing.T) {
	isolate(t)
	srv := fakegh.New(t)
	srv.SetResource("/rate_limit", []byte(rateLimitBody), "")
	setDial(t, noDaemon) // a fresh process knows no bucket

	stdout, stderr, err := run(t, "--api-url", srv.URL, "quota")
	if err != nil {
		t.Fatalf("quota: %v (stderr %q)", err, stderr)
	}
	if n := srv.Calls("/rate_limit"); n != 1 {
		t.Fatalf("/rate_limit calls = %d, want 1", n)
	}
	var q quotaOutput
	if err := json.Unmarshal([]byte(stdout), &q); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if q.SchemaVersion != 1 || !q.Meta.Probed {
		t.Fatalf("unexpected output: %s", stdout)
	}
	if !q.Core.Known || q.Core.Remaining != 4000 || q.Core.Limit != 5000 ||
		!q.GraphQL.Known || q.GraphQL.Remaining != 4500 || !q.Search.Known || q.Search.Remaining != 30 {
		t.Fatalf("unexpected buckets: %s", stdout)
	}
	if strings.Contains(stdout+stderr, "test-token") {
		t.Fatal("token leaked to the output")
	}
}

func TestQuota_PartialSnapshotProbesOnlyUnknown(t *testing.T) {
	srv := fakegh.New(t)
	srv.SetResource("/rate_limit", []byte(rateLimitBody), "")
	snap := map[string]quota.Bucket{
		"core": {Remaining: 10, Limit: 5000, Reset: time.Now().Add(time.Hour), Known: true},
	}
	buckets, probed, err := completeQuota(context.Background(), srv.URL, auth.FromValue("t"), snap)
	if err != nil {
		t.Fatal(err)
	}
	if !probed || srv.Calls("/rate_limit") != 1 {
		t.Fatalf("probed=%v calls=%d, want one probe", probed, srv.Calls("/rate_limit"))
	}
	// The live bucket wins over the probe; the unknown ones come from it.
	if buckets["core"].Remaining != 10 || buckets["graphql"].Remaining != 4500 || !buckets["search"].Known {
		t.Fatalf("unexpected buckets: %+v", buckets)
	}
}
