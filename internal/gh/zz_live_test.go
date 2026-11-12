package gh

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Muriano/OctoGuard/internal/auth"
	"github.com/Muriano/OctoGuard/internal/cache"
	"github.com/Muriano/OctoGuard/internal/quota"
	"github.com/Muriano/OctoGuard/internal/ref"
)

func TestZZLive(t *testing.T) {
	v := os.Getenv("LIVE_TOKEN")
	if v == "" {
		t.Skip()
	}
	tk := auth.Token{Value: v, ID: "live"}
	ch, _ := cache.Open(filepath.Join(t.TempDir(), "c.db"))
	c := NewClient("https://api.github.com", tk, ch, quota.New(time.Now), nil)
	pr := ref.Ref{Host: "github.com", Owner: "Muriano", Repo: "OctoGuard", Number: 22, Kind: ref.PR}
	is := ref.Ref{Host: "github.com", Owner: "Muriano", Repo: "OctoGuard", Number: 6, Kind: ref.Issue}
	for _, x := range []struct {
		r ref.Ref
		w Resource
	}{{pr, PR}, {pr, Reviews}, {pr, Comments}, {is, Issue}, {is, Comments}} {
		rd := &read{c: c, ctx: context.Background(), scope: "s", opt: GetOpts{}}
		b, err := rd.graphQLResource(x.r, x.w)
		t.Logf("GQL %s: err=%v data=%.400s", x.w, err, b)
	}
	for _, w := range []Resource{RequiredChecks, Checks, Runs, PR} {
		res, err := c.Get(context.Background(), pr, w, GetOpts{})
		t.Logf("GET %s via=%s err=%v data=%.300s", w, res.Via, err, res.Data)
	}
}
