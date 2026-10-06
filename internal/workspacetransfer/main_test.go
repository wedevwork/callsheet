package workspacetransfer

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"

	"github.com/wedevwork/callsheet/internal/testkit"
)

// The shared production fixture: one test plane (the production 09a
// workspace manager and git handler on verified TLS) seeded once per test
// process with workspace "ws" (main: two commits, topic: an unrelated
// commit, one planted task ref). Read-only production tests share it, so
// repeated stress runs pay for its durable mutations once.
var shared struct {
	once                 sync.Once
	dir                  string
	tp                   *testPlane
	stop                 func()
	err                  error
	instance             string
	first, second, topic plumbing.Hash
	task                 string
}

func TestMain(m *testing.M) {
	code := m.Run()
	if shared.stop != nil {
		shared.stop()
	}
	if shared.dir != "" {
		os.RemoveAll(shared.dir)
	}
	if processFixtures.dir != "" {
		os.RemoveAll(processFixtures.dir)
	}
	os.Exit(code)
}

// sharedFiles are the shared fixture's two main commits.
var sharedFiles = []map[string]testkit.FileSpec{
	{"a.txt": {Mode: filemode.Regular, Content: []byte("one\n")}, "bin/run": {Mode: filemode.Executable, Content: []byte("#!/bin/sh\n")},
		"l": {Mode: filemode.Symlink, Content: []byte("a.txt")}},
	{"a.txt": {Mode: filemode.Regular, Content: []byte("two\n")}, "bin/run": {Mode: filemode.Executable, Content: []byte("#!/bin/sh\n")},
		"l": {Mode: filemode.Symlink, Content: []byte("a.txt")}, "d/e": {Mode: filemode.Regular, Content: []byte("e")}},
}

func sharedPlane(t testing.TB) *testPlane {
	t.Helper()
	shared.once.Do(func() {
		shared.err = func() error {
			dir, err := os.MkdirTemp("", "callsheet-transfer-shared-")
			if err != nil {
				return err
			}
			shared.dir = dir
			tp, stop, err := newTestPlaneIn(dir)
			if err != nil {
				return err
			}
			shared.tp, shared.stop = tp, stop
			v, err := tp.m().Create(context.Background(), "ws")
			if err != nil {
				return err
			}
			shared.instance = v.Instance
			s := testkit.NewMemoryStore()
			if shared.first, err = testkit.CommitFiles(s, sharedFiles[0], nil, "first"); err != nil {
				return err
			}
			if shared.second, err = testkit.CommitFiles(s, sharedFiles[1], []plumbing.Hash{shared.first}, "second"); err != nil {
				return err
			}
			if shared.topic, err = testkit.CommitFiles(s, map[string]testkit.FileSpec{"t": {Mode: filemode.Regular, Content: []byte("topic")}}, nil, "topic"); err != nil {
				return err
			}
			hc, err := testkit.GitHTTPClient(tp.ca.CertPEM)
			if err != nil {
				return err
			}
			defer hc.CloseIdleConnections()
			r := testkit.GitRemote{URL: tp.url + "/ws/ws.git", Instance: v.Instance, HTTP: hc}
			for ref, h := range map[string]plumbing.Hash{"refs/heads/main": shared.second, "refs/heads/topic": shared.topic} {
				if res := r.Push(context.Background(), s, ref, plumbing.ZeroHash, h); !res.OK() {
					return res.Err
				}
			}
			shared.task = "t_" + strings.Repeat("c", 32)
			return tp.plant("ws", shared.task, shared.first)
		}()
	})
	if shared.err != nil {
		t.Fatalf("shared fixture: %v", shared.err)
	}
	return shared.tp
}
