/*
Copyright 2018 Google, Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package fetcher

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
)

const (
	maxretries = 3

	successBucket     = "success-bucket"
	sfile1            = "sfile1.js"
	sfile2            = "sfile2.jpg"
	sfile3            = "sfile3"
	goodManifest      = "good-manifest.json"
	malformedManifest = "malformed-manifest.json"

	errorBucket   = "error-bucket"
	efile1        = "efile1"
	efile2        = "efile2"
	efile3        = "efile3"
	efile4        = "efile4"
	errorManifest = "error-manifest.json"
	errorZipfile  = "error-source.zip"

	generation int64 = 12345
)

var (
	zeroTime  = time.Time{}
	errNonNil = errors.New("some-error")

	sfile1Contents = []byte("sfile1-contents-a")
	sfile2Contents = []byte("sfile2-contents-aa")
	sfile3Contents = []byte("sfile3-contents-aaa")

	goodManifestContents = []byte(`{
		"sfile1.js":  {"SourceURL": "gs://success-bucket/sfile1.js", "Sha1Sum": ""},
		"sfile2.jpg": {"SourceURL": "gs://success-bucket/sfile2.jpg", "Sha1Sum": ""},
		"sfile3":     {"SourceURL": "gs://success-bucket/sfile3", "Sha1Sum": ""}
	}`)
	malformedManifestContents = []byte(`{
		"sfile1.js": {"SourceURL": "gs://success-bucket/sfile1.js", "Sha1Sum": ""},
		"sfile2.jpg": {"SourceURL": "gs://succ`)

	errGCSNewReader = fmt.Errorf("instrumented GCS NewReader error")
	errGCSRead      = fmt.Errorf("instrumented GCS Read err")
	errGCSSlowRead  = fmt.Errorf("instrumented GCS slow Read err")
	errRename       = fmt.Errorf("instrumented os.Rename error")
	errChmod        = fmt.Errorf("instrumented os.Chmod error")
	errCreate       = fmt.Errorf("instrumented os.Create error")
	errMkdirAll     = fmt.Errorf("instrumented os.MkdirAll error")
	errOpen         = fmt.Errorf("instrumented os.Open error")
	errGCS403       = fmt.Errorf("instrumented GCS AccessDenied error")
)

type fakeGCSErrorReader struct {
	err   error
	sleep time.Duration
}

func (f fakeGCSErrorReader) Read([]byte) (int, error) {
	time.Sleep(f.sleep)
	return 0, f.err
}

type fakeGCSResponse struct {
	content []byte
	err     error
}

// fakeGCS allows us to simulate errors when interacting with GCS.
type fakeGCS struct {
	t       *testing.T
	objects map[string]fakeGCSResponse
}

func (f *fakeGCS) NewReader(context context.Context, bucket, object string) (io.ReadCloser, error) {
	f.t.Helper()
	name := formatGCSName(bucket, object, generation)

	response, ok := f.objects[name]
	if !ok {
		f.t.Fatalf("no %q in instrumented responses", name)
		return nil, nil
	}

	if response.err == errGCSNewReader {
		return ioutil.NopCloser(bytes.NewReader([]byte(""))), response.err
	}

	if response.err == errGCS403 {
		message := "<Xml><Code>AccessDenied</Code><Details>some@robot has no access.</Details></Xml>"
		err := &googleapi.Error{
			Code: 403,
			Body: message,
		}
		return ioutil.NopCloser(bytes.NewReader([]byte(""))), err
	}

	if response.err == errGCSRead {
		return ioutil.NopCloser(fakeGCSErrorReader{err: response.err}), nil
	}

	if response.err == errGCSSlowRead {
		return ioutil.NopCloser(fakeGCSErrorReader{sleep: 1 * time.Second}), nil
	}

	if response.err != nil {
		f.t.Fatalf("unexpected error type %v", response.err)
	}

	return ioutil.NopCloser(bytes.NewReader(response.content)), nil
}

// fakeOS raises errors if configures, otherwise simply passes
// through to the normal os package.
type fakeOS struct {
	errorsRename   int
	errorsChmod    int
	errorsCreate   int
	errorsMkdirAll int
	errorsOpen     int
}

func (f *fakeOS) Rename(oldpath, newpath string) error {
	if f.errorsRename > 0 {
		f.errorsRename--
		return errRename
	}
	return os.Rename(oldpath, newpath)
}

func (f *fakeOS) Chmod(name string, mode os.FileMode) error {
	if f.errorsChmod > 0 {
		f.errorsChmod--
		return errChmod
	}
	return os.Chmod(name, mode)
}

func (f *fakeOS) Create(name string) (*os.File, error) {
	if f.errorsCreate > 0 {
		f.errorsCreate--
		return nil, errCreate
	}

	return os.Create(name)
}

func (f *fakeOS) MkdirAll(path string, perm os.FileMode) error {
	if f.errorsMkdirAll > 0 {
		f.errorsMkdirAll--
		return errMkdirAll
	}
	return os.MkdirAll(path, perm)
}

func (f *fakeOS) Open(name string) (*os.File, error) {
	if f.errorsOpen > 0 {
		f.errorsOpen--
		return nil, errOpen
	}
	return os.Open(name)
}

func (*fakeOS) RemoveAll(path string) error {
	return os.RemoveAll(path)
}

type testContext struct {
	gf      *Fetcher
	gcs     *fakeGCS
	os      *fakeOS
	workDir string
}

func buildTestContext(t *testing.T) (tc *testContext, teardown func()) {
	t.Helper()

	// Set up a temp directory for each test so it's easy to clean up.
	workDir, err := ioutil.TempDir("", "fetcher")
	if err != nil {
		t.Fatal(err)
	}

	fakeos := &fakeOS{}

	gcs := &fakeGCS{
		t: t,
		objects: map[string]fakeGCSResponse{
			formatGCSName(successBucket, sfile1, generation):            {content: sfile1Contents},
			formatGCSName(successBucket, sfile2, generation):            {content: sfile2Contents},
			formatGCSName(successBucket, sfile3, generation):            {content: sfile3Contents},
			formatGCSName(errorBucket, efile1, generation):              {err: errGCSNewReader},
			formatGCSName(errorBucket, efile2, generation):              {err: errGCSRead},
			formatGCSName(errorBucket, efile3, generation):              {err: errGCSSlowRead},
			formatGCSName(errorBucket, efile4, generation):              {err: errGCS403},
			formatGCSName(successBucket, goodManifest, generation):      {content: goodManifestContents},
			formatGCSName(successBucket, malformedManifest, generation): {content: malformedManifestContents},
			formatGCSName(errorBucket, errorManifest, generation):       {err: errGCSRead},
		},
	}

	gf := &Fetcher{
		GCS:         gcs,
		OS:          fakeos,
		DestDir:     workDir,
		StagingDir:  filepath.Join(workDir, ".staging/"),
		CreatedDirs: make(map[string]bool),
		Bucket:      successBucket,
		Object:      goodManifest,
		TimeoutGCS:  true,
		WorkerCount: 2,
		Retries:     maxretries,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
	}

	return &testContext{
			workDir: workDir,
			os:      fakeos,
			gcs:     gcs,
			gf:      gf,
		},
		func() {
			if err := os.RemoveAll(workDir); err != nil {
				t.Logf("Failed to remove working dir %q, continuing.", workDir)
			}
		}
}

func TestFetchObjectOnceStoresFile(t *testing.T) {
	tc, teardown := buildTestContext(t)
	defer teardown()

	j := job{bucket: successBucket, object: sfile1}
	dest := filepath.Join(tc.workDir, "sfile1.tmp")

	result := tc.gf.fetchObjectOnce(context.Background(), j, dest, make(chan struct{}, 1))

	if result.err != nil {
		t.Errorf("fetchObjectOnce() result.err got %v, want nil", result.err)
	}
	if int(result.size) != len(sfile1Contents) {
		t.Errorf("fetchObjectOnce() result.size got %d, want %d", result.size, len(sfile1Contents))
	}

	got, err := ioutil.ReadFile(dest)
	if err != nil {
		t.Fatalf("ReadFile(%v) got %v, want nil", dest, err)
	}
	if !bytes.Equal(got, sfile1Contents) {
		t.Fatalf("ReadFile(%v) got %v, want %v", dest, got, sfile1Contents)
	}
}

func TestGCSAccessDenied(t *testing.T) {
	tc, teardown := buildTestContext(t)
	defer teardown()
	j := job{bucket: errorBucket, object: efile4}
	result := tc.gf.fetchObjectOnce(context.Background(), j, filepath.Join(tc.workDir, "efile4.tmp"), make(chan struct{}, 1))
	if result.err == nil {
		t.Fatalf("fetchObjectOnce did not fail, got err=nil, want err!=nil")
	}
	if err, ok := result.err.(*permissionError); ok {
		want := `Access to bucket error-bucket denied. You must grant Storage Object Viewer permission to some@robot. If you are using VPC Service Controls, you must also grant it access to your service perimeter.`
		if err.Error() != want {
			t.Fatalf("incorrect error message, got %q, want %q", err.Error(), want)
		}
	} else {
		t.Fatalf("got err=%q, want permissionError", result.err)
	}
}

func TestFetchObjectOnceFailureModes(t *testing.T) {

	// GCS NewReader failure
	tc, teardown := buildTestContext(t)
	j := job{bucket: errorBucket, object: efile1}
	result := tc.gf.fetchObjectOnce(context.Background(), j, filepath.Join(tc.workDir, "efile1.tmp"), make(chan struct{}, 1))
	if result.err == nil || !strings.HasSuffix(result.err.Error(), errGCSNewReader.Error()) {
		t.Errorf("fetchObjectOnce did not fail correctly, got err=%v, want err=%v", result.err, errGCSNewReader)
	}
	teardown()

	// Failure due to cancellation
	tc, teardown = buildTestContext(t)
	breaker := make(chan struct{}, 1)
	breaker <- struct{}{}
	j = job{bucket: successBucket, object: sfile1}
	result = tc.gf.fetchObjectOnce(context.Background(), j, filepath.Join(tc.workDir, "sfile1.tmp"), breaker)
	if result.err == nil || result.err != errGCSTimeout {
		t.Errorf("fetchObjectOnce did not fail correctly, got err=%v, want err=%v", result.err, errGCSTimeout)
	}
	teardown()

	// os.Create failure
	tc, teardown = buildTestContext(t)
	tc.os.errorsCreate = 1
	j = job{bucket: successBucket, object: sfile1}
	result = tc.gf.fetchObjectOnce(context.Background(), j, filepath.Join(tc.workDir, "sfile1.tmp"), make(chan struct{}, 1))
	if result.err == nil || !strings.HasSuffix(result.err.Error(), errCreate.Error()) {
		t.Errorf("fetchObjectOnce did not fail correctly, got err=%v, want err=%v", result.err, errCreate)
	}
	teardown()

	// GCS Copy failure
	tc, teardown = buildTestContext(t)
	j = job{bucket: errorBucket, object: efile2}
	result = tc.gf.fetchObjectOnce(context.Background(), j, filepath.Join(tc.workDir, "efile2.tmp"), make(chan struct{}, 1))
	if result.err == nil || !strings.HasSuffix(result.err.Error(), errGCSRead.Error()) {
		t.Errorf("fetchObjectOnce did not fail correctly, got err=%v, want err=%v", result.err, errGCSRead)
	}
	teardown()

	// SHA checksum failure
	// TODO(jasonco): Add a SHA checksum failure test
}

func TestFetchObjectOnceWithTimeoutSucceeds(t *testing.T) {
	tc, teardown := buildTestContext(t)
	defer teardown()

	j := job{bucket: successBucket, object: sfile1}
	timeout := 10 * time.Second
	dest := filepath.Join(tc.workDir, "sfile1.tmp")

	n, err := tc.gf.fetchObjectOnceWithTimeout(context.Background(), j, timeout, dest)
	if err != nil || int(n) != len(sfile1Contents) {
		t.Errorf("fetchObjectOnceWithTimeout() got (%v, %v), want (%v, %v)", n, err, nil, len(sfile1Contents))
	}
}

func TestFetchObjectOnceWithTimeoutFailsOnTimeout(t *testing.T) {
	tc, teardown := buildTestContext(t)
	defer teardown()

	j := job{bucket: errorBucket, object: efile3} // efile3 is a slow GCS read
	timeout := 100 * time.Millisecond
	dest := filepath.Join(tc.workDir, "efile3.tmp")

	if _, err := tc.gf.fetchObjectOnceWithTimeout(context.Background(), j, timeout, dest); err == nil {
		t.Errorf("fetchObjectOnceWithTimeout() got err=nil, want err=%v", errGCSTimeout)
	}
}

func TestFetchObjectSucceeds(t *testing.T) {
	tc, teardown := buildTestContext(t)
	defer teardown()

	j := job{bucket: successBucket, object: sfile1, filename: "localfile.txt"}
	report := tc.gf.fetchObject(context.Background(), j)

	if report.job != j {
		t.Errorf("report.job got %v, want %v", report.job, j)
	}
	if !report.success {
		t.Errorf("report.success got false, want true")
	}
	if report.err != nil {
		t.Errorf("report.err got %v, want nil", report.err)
	}
	if report.started == zeroTime {
		t.Errorf("report.started got %v, want report.started > %v", report.started, zeroTime)
	}
	if report.completed == zeroTime {
		t.Errorf("report.completed got %v, want report.completed > %v", report.completed, zeroTime)
	}
	if int(report.size) != len(sfile1Contents) {
		t.Errorf("report.size got %v, want %v", report.size, len(sfile1Contents))
	}
	if report.finalname == "" {
		t.Errorf("report.finalname got empty string, want non-empty string")
	}
	if len(report.attempts) != 1 {
		t.Fatalf("len(report.attempts) got %d, want 1", len(report.attempts))
	}

	attempt := report.attempts[0]
	if attempt.started == zeroTime {
		t.Errorf("attempt.started got %v, want attempt.started > %v", attempt.started, zeroTime)
	}
	if attempt.duration == 0 {
		t.Errorf("attempt.duration got %v, want attempt.duration>0", attempt.duration)
	}
	if attempt.err != nil {
		t.Errorf("attempt.err got %v, want nil", attempt.err)
	}

	got, err := ioutil.ReadFile(report.finalname)
	if err != nil {
		t.Fatalf("ReadFile(%v) got %v, want nil", report.finalname, err)
	}
	if !bytes.Equal(got, sfile1Contents) {
		t.Fatalf("ReadFile(%v) got %v, want %v", report.finalname, got, sfile1Contents)
	}
}

func TestFetchObjectRetriesUntilSuccess(t *testing.T) {
	tc, teardown := buildTestContext(t)
	defer teardown()
	tc.os.errorsCreate = 1 // first create fails, second succeeds

	j := job{bucket: successBucket, object: sfile1, filename: "localhost.txt"}
	report := tc.gf.fetchObject(context.Background(), j)

	if !report.success {
		t.Errorf("report.success got false, want true")
	}
	if report.err != nil {
		t.Errorf("report.err got %v, want nil", report.err)
	}

	if len(report.attempts) != 2 {
		t.Fatalf("len(report.attempts) got %d, want 2", len(report.attempts))
	}

	attempt1 := report.attempts[0]
	if attempt1.err == nil {
		t.Errorf("attempt.err got %v, want non-nil", attempt1.err)
	}

	attempt2 := report.attempts[1]
	if attempt2.err != nil {
		t.Errorf("attempt.err got %v, want nil", attempt2.err)
	}

	got, err := ioutil.ReadFile(report.finalname)
	if err != nil {
		t.Fatalf("ReadFile(%v) got %v, want nil", report.finalname, err)
	}
	if !bytes.Equal(got, sfile1Contents) {
		t.Fatalf("ReadFile(%v) got %v, want %v", report.finalname, got, sfile1Contents)
	}
}

func TestFetchObjectRetriesMaxTimes(t *testing.T) {
	tc, teardown := buildTestContext(t)
	defer teardown()
	tc.os.errorsCreate = maxretries + 1 // create continually fails until max reached

	filename := "localfile.txt"
	j := job{bucket: successBucket, object: sfile1, filename: filename}

	report := tc.gf.fetchObject(context.Background(), j)

	if report.success {
		t.Errorf("report.success got true, want false")
	}
	if report.err == nil {
		t.Errorf("report.err got %v, want non-nil", report.err)
	}
	if report.finalname != "" {
		t.Errorf("report.finalname got %v want empty string", report.finalname)
	}
	if len(report.attempts) != maxretries+1 {
		t.Fatalf("len(report.attempts) got %d, want %d", len(report.attempts), maxretries+1)
	}

	last := report.attempts[len(report.attempts)-1]
	if last.err == nil {
		t.Errorf("attempt.err got %v, want non-nil", last.err)
	}

	localfile := filepath.Join(tc.gf.DestDir, filename)
	if _, err := os.Stat(localfile); !os.IsNotExist(err) {
		t.Errorf("file %q exists, want not exists", localfile)
	}
}

func TestFetchObjectRetriesOnFolderCreationError(t *testing.T) {
	tc, teardown := buildTestContext(t)
	defer teardown()
	tc.os.errorsMkdirAll = 1

	j := job{bucket: successBucket, object: sfile1, filename: "localfile.txt"}
	report := tc.gf.fetchObject(context.Background(), j)

	if !report.success {
		t.Errorf("report.success got false, want true")
	}
	if report.err != nil {
		t.Errorf("report.err got %v, want nil", report.err)
	}

	if len(report.attempts) != 2 {
		t.Fatalf("len(report.attempts) got %d, want 2", len(report.attempts))
	}
	first := report.attempts[0]
	if first.err == nil || !strings.Contains(first.err.Error(), errMkdirAll.Error()) {
		t.Errorf("attempt.err got %v, want Contains(%v)", first.err, errMkdirAll)
	}

	got, err := ioutil.ReadFile(report.finalname)
	if err != nil {
		t.Fatalf("ReadFile(%v) got %v, want nil", report.finalname, err)
	}
	if !bytes.Equal(got, sfile1Contents) {
		t.Fatalf("ReadFile(%v) got %v, want %v", report.finalname, got, sfile1Contents)
	}
}

func TestFetchObjectRetriesOnFetchFail(t *testing.T) {
	tc, teardown := buildTestContext(t)
	defer teardown()
	tc.os.errorsCreate = 1 // Invoked when fetching the file.

	j := job{bucket: successBucket, object: sfile1, filename: "localfile.txt"}
	report := tc.gf.fetchObject(context.Background(), j)

	if !report.success {
		t.Errorf("report.success got false, want true")
	}
	if report.err != nil {
		t.Errorf("report.err got %v, want nil", report.err)
	}

	if len(report.attempts) != 2 {
		t.Fatalf("len(report.attempts) got %d, want 2", len(report.attempts))
	}
	first := report.attempts[0]
	if first.err == nil || !strings.Contains(first.err.Error(), errCreate.Error()) {
		t.Errorf("attempt.err got %v, want Contains(%v)", first.err, errCreate)
	}

	got, err := ioutil.ReadFile(report.finalname)
	if err != nil {
		t.Fatalf("ReadFile(%v) got %v, want nil", report.finalname, err)
	}
	if !bytes.Equal(got, sfile1Contents) {
		t.Fatalf("ReadFile(%v) got %v, want %v", report.finalname, got, sfile1Contents)
	}
}

func TestFetchObjectRetriesOnRenameFailure(t *testing.T) {
	tc, teardown := buildTestContext(t)
	defer teardown()
	tc.os.errorsRename = 1

	j := job{bucket: successBucket, object: sfile1, filename: "localfile.txt"}
	report := tc.gf.fetchObject(context.Background(), j)

	if !report.success {
		t.Errorf("report.success got false, want true")
	}
	if report.err != nil {
		t.Errorf("report.err got %v, want nil", report.err)
	}

	if len(report.attempts) != 2 {
		t.Fatalf("len(report.attempts) got %d, want 2", len(report.attempts))
	}
	first := report.attempts[0]
	if first.err == nil || !strings.Contains(first.err.Error(), errRename.Error()) {
		t.Errorf("attempt.err got %v, want Contains(%v)", first.err, errRename)
	}

	got, err := ioutil.ReadFile(report.finalname)
	if err != nil {
		t.Fatalf("ReadFile(%v) got %v, want nil", report.finalname, err)
	}
	if !bytes.Equal(got, sfile1Contents) {
		t.Fatalf("ReadFile(%v) got %v, want %v", report.finalname, got, sfile1Contents)
	}
}

func TestFetchObjectRetriesOnChmodFailure(t *testing.T) {
	tc, teardown := buildTestContext(t)
	defer teardown()
	tc.os.errorsChmod = 1

	j := job{bucket: successBucket, object: sfile1, filename: "localfile.txt"}
	report := tc.gf.fetchObject(context.Background(), j)

	if !report.success {
		t.Errorf("report.success got false, want true")
	}
	if report.err != nil {
		t.Errorf("report.err got %v, want nil", report.err)
	}

	if len(report.attempts) != 2 {
		t.Fatalf("len(report.attempts) got %d, want 2", len(report.attempts))
	}
	first := report.attempts[0]
	if first.err == nil || !strings.Contains(first.err.Error(), errChmod.Error()) {
		t.Errorf("attempt.err got %v, want Contains(%v)", first.err, errChmod)
	}

	got, err := ioutil.ReadFile(report.finalname)
	if err != nil {
		t.Fatalf("ReadFile(%v) got %v, want nil", report.finalname, err)
	}
	if !bytes.Equal(got, sfile1Contents) {
		t.Fatalf("ReadFile(%v) got %v, want %v", report.finalname, got, sfile1Contents)
	}
}

func TestDoWork(t *testing.T) {
	tc, teardown := buildTestContext(t)
	defer teardown()

	files := []string{sfile1, sfile2, sfile3}
	sort.Strings(files)

	// Add n jobs
	todo := make(chan job, len(files))
	results := make(chan jobReport, len(files))
	for i, file := range files {
		todo <- job{bucket: successBucket, object: file, filename: fmt.Sprintf("sfile-%d", i)}
	}

	// Process the jobs
	go tc.gf.doWork(context.Background(), todo, results)

	// Get n reports
	var gotFiles []string
	for range files {
		report := <-results
		if report.err != nil {
			t.Errorf("file %q: report.err got %v, want nil", report.job.filename, report.err)
		}
		if _, err := os.Stat(report.finalname); os.IsNotExist(err) {
			t.Errorf("file %q: does not exist, but it should exist", report.finalname)
		}
		gotFiles = append(gotFiles, report.job.object)
	}

	// Ensure there is nothing more in the results channel
	select {
	case report, ok := <-results:
		if ok {
			t.Errorf("unexpected report found on channel: %v", report)
		} else {
			close(todo)
		}
	default:
	}
	close(results)

	sort.Strings(gotFiles)
	if !reflect.DeepEqual(gotFiles, files) {
		t.Fatalf("processJobs files got %v, want %v", gotFiles, files)
	}
}

func TestProcessJobs(t *testing.T) {
	tc, teardown := buildTestContext(t)
	defer teardown()
	tc.os.errorsCreate = 1 // Provoke one retry

	jobs := []job{
		{bucket: successBucket, object: sfile1, filename: "sfile1"},
		{bucket: successBucket, object: sfile2, filename: "sfile2"},
		{bucket: successBucket, object: sfile3, filename: "sfile3"},
	}

	stats := tc.gf.processJobs(context.Background(), jobs)

	if !stats.success {
		t.Errorf("processJobs() stats.success got false, want true")
	}
	if len(stats.errs) != 0 {
		t.Errorf("processJobs() stats.errs got %v, want {}", stats.errs)
	}
	if stats.files != len(jobs) {
		t.Errorf("processJobs stats.files got %d, want %d", stats.files, len(jobs))
	}

	wantSize := len(sfile1Contents) + len(sfile2Contents) + len(sfile3Contents)
	if int(stats.size) != wantSize {
		t.Errorf("processJobs() stats.size got %d, want %d", stats.size, wantSize)
	}
	if stats.retries != 1 {
		t.Errorf("processJobs() stats.retries got %d, want 1", stats.retries)
	}
}

func TestFetchFromManifestPermissionDenied(t *testing.T) {
	if os.Getenv("subprocess") == "1" {
		tc, teardown := buildTestContext(t)
		defer teardown()

		tc.gf.Bucket = errorBucket
		tc.gf.Object = efile4

		tc.gf.fetchFromManifest(context.Background())
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFetchFromManifestPermissionDenied$")
	cmd.Env = append(os.Environ(), "subprocess=1")
	err := cmd.Run()
	if err == nil {
		t.Fatal("fetchFromManifest() unexpectedly exited with success")
	}
	exitError, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatal("fetchFromManifest() unexpectedly exited with unknown error")
	}
	exitCode := exitError.ExitCode()
	if exitCode != permissionDeniedExitStatus {
		t.Fatalf("fetchFromManifest() exited with wrong status, got %v, want %v", exitCode, permissionDeniedExitStatus)
	}
}

func TestFetchFromZipPermissionDenied(t *testing.T) {
	if os.Getenv("subprocess") == "1" {
		tc, teardown := buildTestContext(t)
		defer teardown()

		tc.gf.Bucket = errorBucket
		tc.gf.Object = efile4

		tc.gf.fetchFromZip(context.Background())
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFetchFromZipPermissionDenied$")
	cmd.Env = append(os.Environ(), "subprocess=1")
	err := cmd.Run()
	if err == nil {
		t.Fatal("fetchFromZip() unexpectedly exited with success")
	}
	exitError, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatal("fetchFromZip() unexpectedly exited with unknown error")
	}
	exitCode := exitError.ExitCode()
	if exitCode != permissionDeniedExitStatus {
		t.Fatalf("fetchFromZip() exited with wrong status, got %v, want %v", exitCode, permissionDeniedExitStatus)
	}
}

func TestFetchFromTarGzPermissionDenied(t *testing.T) {
	if os.Getenv("subprocess") == "1" {
		tc, teardown := buildTestContext(t)
		defer teardown()

		tc.gf.Bucket = errorBucket
		tc.gf.Object = efile4

		tc.gf.fetchFromTarGz(context.Background())
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFetchFromTarGzPermissionDenied$")
	cmd.Env = append(os.Environ(), "subprocess=1")
	err := cmd.Run()
	if err == nil {
		t.Fatal("fetchFromTarGz() unexpectedly exited with success")
	}
	exitError, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatal("fetchFromTarGz() unexpectedly exited with unknown error")
	}
	exitCode := exitError.ExitCode()
	if exitCode != permissionDeniedExitStatus {
		t.Fatalf("fetchFromTarGz() exited with wrong status, got %v, want %v", exitCode, permissionDeniedExitStatus)
	}
}

func TestFetchFromManifestSuccess(t *testing.T) {
	tc, teardown := buildTestContext(t)
	defer teardown()

	tc.gf.Bucket = successBucket
	tc.gf.Object = goodManifest

	err := tc.gf.fetchFromManifest(context.Background())
	if err != nil {
		t.Errorf("fetchFromManifest() got %v, want nil", err)
	}

	// Check that enough files are present
	infos, err := ioutil.ReadDir(tc.gf.DestDir)
	if err != nil {
		t.Fatalf("ReadDir(%v) err = %v, want nil", tc.gf.DestDir, err)
	}
	if len(infos) != 3 {
		t.Errorf("ReadDir(%v) len(fileinfos)=%v, want 4", tc.gf.DestDir, len(infos))
	}
}

func TestFetchFromManifestManifestFetchFailed(t *testing.T) {
	tc, teardown := buildTestContext(t)
	defer teardown()

	tc.gf.Bucket = errorBucket
	tc.gf.Object = errorManifest

	err := tc.gf.fetchFromManifest(context.Background())
	if err == nil || !strings.Contains(err.Error(), errGCSRead.Error()) {
		t.Errorf("fetchFromManifest() err=%v, want contains %v", err, errGCSRead)
	}
}

func TestFetchFromManifestManifestJSONDeserializtionFailed(t *testing.T) {
	tc, teardown := buildTestContext(t)
	defer teardown()

	tc.gf.Bucket = successBucket
	tc.gf.Object = malformedManifest

	wantErrStr := "decoding JSON from manifest file"
	err := tc.gf.fetchFromManifest(context.Background())
	if err == nil || !strings.Contains(err.Error(), wantErrStr) {
		t.Errorf("fetchFromManifest() err=%v, want contains %q", err, wantErrStr)
	}
}

func TestFetchFromManifestManifestFileReadFailed(t *testing.T) {
	tc, teardown := buildTestContext(t)
	defer teardown()
	tc.os.errorsOpen = 1 // Error returned when trying to open the downloaded manifest file

	err := tc.gf.fetchFromManifest(context.Background())
	if err == nil || !strings.Contains(err.Error(), errOpen.Error()) {
		t.Errorf("fetchFromManifest() err=%v, want contains %v", err, errOpen)
	}
}

func TestTimeout(t *testing.T) {
	tests := []struct {
		filename string
		retrynum int
		want     time.Duration
	}{
		{"source.js", 0, sourceTimeout[0]},
		{"source.js", 1, sourceTimeout[1]},
		{"source.js", 2, defaultTimeout},
		{"not-source.mpg", 0, notSourceTimeout[0]},
		{"not-source.mpg", 1, notSourceTimeout[1]},
		{"not-source.mpg", 2, defaultTimeout},
		{"no-extension", 0, notSourceTimeout[0]},
		{"no-extension", 1, notSourceTimeout[1]},
		{"no-extension", 2, defaultTimeout},
	}
	tc, teardown := buildTestContext(t)
	defer teardown()
	for _, test := range tests {
		got := tc.gf.timeout(test.filename, test.retrynum)
		if got != test.want {
			t.Errorf("getTimeout(%v, %v) got %v, want %v", test.filename, test.retrynum, got, test.want)
		}
	}
}

func TestUnzip(t *testing.T) {
	type zipEntry struct {
		name    string
		content string
		mode    os.FileMode
		modeStr string // Used later for comparison to avoid extended attributes introduced by zip package.
	}

	tests := []struct {
		name    string
		entries []zipEntry
	}{
		{
			name: "empty zip",
		},
		{
			name: "single file",
			entries: []zipEntry{
				{name: "file.txt", content: "file.txt content", mode: 0644},
			},
		},
		{
			name: "multiple files",
			entries: []zipEntry{
				{name: "file.txt", content: "file.txt content", mode: 0644},
				{name: "another/", mode: 0755},
				{name: "another/file.txt", content: "another file-2.txt content", mode: 0644},
			},
		},
		{
			name: "single directory",
			entries: []zipEntry{
				{name: "directory/", mode: 0755},
			},
		},
		{
			name: "multiple directories",
			entries: []zipEntry{
				{name: "some/", mode: 0755},
				{name: "some/directory/", mode: 0755},
			},
		},
		{
			name: "complex",
			entries: []zipEntry{
				{name: "file.txt", content: "file.txt content", mode: 0644},
				{name: "some/", mode: 0755},
				{name: "some/directory/", mode: 0755},
				{name: "some/directory/file.txt", content: "another file-2.txt content", mode: 0644},
				{name: "some/other-directory/", mode: 0755},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmp, err := ioutil.TempDir("", "gcs-fetcher-unzip-")
			if err != nil {
				t.Fatalf("Creating temp dir: %v", err)
			}
			defer func() {
				if err := os.RemoveAll(tmp); err != nil {
					t.Fatalf("Removing temp dir %s: %v", tmp, err)
				}
			}()

			// Create a zipfile.
			want := make(map[string]zipEntry)
			zipfile := filepath.Join(tmp, "source.zip")
			outfile, err := os.OpenFile(zipfile, os.O_WRONLY|os.O_CREATE, 0666)
			if err != nil {
				t.Fatalf("Creating zipfile: %v", err)
			}
			writer := zip.NewWriter(outfile)
			for _, entry := range tc.entries {
				fh := &zip.FileHeader{Name: entry.name}

				m := entry.mode
				if strings.HasSuffix(entry.name, "/") {
					m = m + os.ModeDir
				}
				fh.SetMode(m)

				want[entry.name] = zipEntry{name: entry.name, content: entry.content, modeStr: m.String()}

				f, err := writer.CreateHeader(fh)
				if err != nil {
					t.Fatalf("Creating entry %s in zipfile: %v", entry.name, err)
				}

				if entry.content == "" {
					continue
				}

				if _, err = f.Write([]byte(entry.content)); err != nil {
					t.Fatalf("Writing content for file %s in zipfile: %v", entry.name, err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatalf("Closing zipfile: %v", err)
			}

			// Create an unzip directory.
			dest := filepath.Join(tmp, "unzip")
			if err := os.MkdirAll(dest, 0777); err != nil {
				t.Fatalf("Creating unzip dir: %v", err)
			}

			// Unzip the archive (this is the function under test).
			_, err = unzip(zipfile, dest)

			// Walk the unzip folder and store the unzipped results for comparison.
			got := make(map[string]zipEntry)
			err = filepath.Walk(dest, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}

				// Skip the root directory.
				if path == dest {
					return nil
				}

				e := zipEntry{name: strings.TrimPrefix(path, dest+"/"), modeStr: info.Mode().String()}

				if info.IsDir() {
					e.name = e.name + "/"
				} else {
					// Read the file contents.
					b, err := ioutil.ReadFile(path)
					if err != nil {
						return fmt.Errorf("reading file %s: %v", path, err)
					}
					e.content = string(b)
				}

				got[e.name] = e
				return nil
			})
			if err != nil {
				t.Fatalf("walking unzip folder %s: %v", dest, err)
			}

			if !reflect.DeepEqual(got, want) {
				t.Errorf("unzipped files do not match, got %#v, want %#v", got, want)
			}
		})
	}
}

