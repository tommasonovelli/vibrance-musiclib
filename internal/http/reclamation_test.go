package http

import (
	"bytes"
	"context"
	"errors"
	nethttp "net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"musiclib/internal/blobstore"
	"musiclib/internal/catalog"
	"musiclib/internal/failpoint"
	"musiclib/internal/reclaim"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
)

func TestReclamationDownloadBeforeOpenAndFatalFence(t *testing.T) {
	url := pgtest.EmptyDB(t)
	direct := pgtest.Pool(t, url)
	if err := store.Migrate(t.Context(), direct); err != nil {
		t.Fatal(err)
	}
	proxy := pgtest.NewProxy(t, url)
	e := newEnvOn(t, pgtest.Pool(t, proxy.URL), true)
	gate := reclaim.NewAdmission()
	e.api.admission = gate
	a := e.seedReal("Artist", "Reclamation test", catalog.FormatFLAC)
	id := store.NewID()
	e.exec(`INSERT INTO settings (id,store_id) VALUES (1,$1)`, id)
	payload := []byte("genuine fixture attachment for retained download")
	up := e.must(upload("POST", albumPath(a.id)+"/attachments?path=retained.txt", ETag(KindAlbum, a.id, 1), payload), nethttp.StatusCreated)
	download := up.header.Get("Location")
	attach := strings.TrimSuffix(download, "/content")
	hash := sha(payload)
	found := make(chan struct{})
	unwait := make(chan struct{})
	callback := make(chan struct{})
	callbackDone := make(chan struct{})
	releaseDownload := sync.OnceFunc(func() { close(unwait) })
	releaseCallback := sync.OnceFunc(func() { close(callbackDone) })
	defer releaseDownload()
	defer releaseCallback()
	e.hooks.Set(func(p failpoint.Point) error {
		if p.Name == "download_before_open" {
			close(found)
			<-unwait
		}
		return nil
	})
	e.api.fatal = func(err error) {
		if store.Code(err) != store.CodeCommitUncertain {
			t.Errorf("fatal callback = %v", err)
		}
		close(callback)
		<-callbackDone
	}
	answer := make(chan dl, 1)
	go func() { answer <- e.download("GET", download) }()
	wait(t, found, "download has looked up the original before open")

	proxy.LoseNextCommitAck()
	failure := make(chan resp, 1)
	go func() {
		failure <- e.do(req{method: "DELETE", path: attach, ifMatch: ETag(KindAlbum, a.id, 2)})
	}()
	wait(t, callback, "DELETE hit its fatal callback before replying")
	if _, err := gate.Enter(t.Context()); !errors.Is(err, reclaim.ErrStopped) {
		t.Fatalf("fatal fence was late: %v", err)
	}
	if _, err := store.New(direct).ReadOriginalReclamation(t.Context(), hash); err != nil {
		t.Fatalf("real DELETE did not produce a pending hash: %v", err)
	}
	// A fresh collector must still be excluded by the process's fenced gate.
	collector := reclaim.New(direct, id, e.originals, gate, nil, nil)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	collector.Run(ctx)
	retained, err := e.blobs.Verify(t.Context(), hash)
	if err != nil || retained != int64(len(payload)) {
		t.Fatalf("fenced removal changed original: %d %v", retained, err)
	}
	releaseCallback()
	select {
	case got := <-failure:
		if got.status != nethttp.StatusServiceUnavailable || got.code() != store.CodeCommitUncertain {
			t.Fatalf("uncertain DELETE = %d %s", got.status, got.raw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fatal DELETE did not return")
	}
	// The DELETE has released its permit. The admitted download alone
	// must still keep shutdown from draining originals admission.
	drained := make(chan struct{})
	go func() {
		gate.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		t.Fatal("originals admission drained before the download opened")
	case <-time.After(100 * time.Millisecond):
	}

	releaseDownload()
	select {
	case got := <-answer:
		if got.status != nethttp.StatusOK || !bytes.Equal(got.body, payload) {
			t.Fatalf("retained download = %d %q", got.status, got.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("download did not close")
	}
	wait(t, drained, "admission drained after the download")
}

func TestReclamationUploadPreservesFatalRegistrationError(t *testing.T) {
	e := &env{t: t}
	e.openStore()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	fatal := &store.Error{Code: store.CodeConnectionLost, Msg: "synthetic registration lookup failure"}
	blobs, err := blobstore.NewRegistered(e.originals, e.work, func(context.Context, string) (bool, error) {
		cancel()
		return false, fatal
	})
	if err != nil {
		t.Fatal(err)
	}
	h := handlers{blobs: blobs}
	_, err = h.putUpload(ctx, strings.NewReader("synthetic upload"), MaxAttachmentBytes)
	if !errors.Is(err, fatal) {
		t.Fatalf("upload replaced the fatal registration error: %v", err)
	}
}
