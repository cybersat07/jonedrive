package graph

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/jstaf/onedriver/logger"
	log "github.com/sirupsen/logrus"
)

const (
	mountLoc = "mount"
	TestDir  = mountLoc + "/onedriver_tests"
	DeltaDir = TestDir + "/delta"
)

var auth *Auth
var fsCache *Cache // used to inject bad content into the fs for some tests

// Tests are done in the main project directory with a mounted filesystem to
// avoid having to repeatedly recreate auth_tokens.json and juggle multiple auth
// sessions.
func TestMain(m *testing.M) {
	os.Chdir("..")
	// attempt to unmount regardless of what happens (in case previous tests
	// failed and didn't clean themselves up)
	exec.Command("fusermount", "-uz", mountLoc).Run()
	os.Mkdir(mountLoc, 0755)
	// wipe all cached data from previous tests
	toDelete, _ := filepath.Glob("test*.db")
	for _, db := range toDelete {
		os.Remove(db)
	}

	logFile, _ := os.OpenFile("fusefs_tests.log", os.O_TRUNC|os.O_CREATE|os.O_RDWR, 0644)
	defer logFile.Close()
	log.SetOutput(logFile)
	log.SetReportCaller(true)
	log.SetFormatter(logger.LogrusFormatter())
	log.SetLevel(log.DebugLevel)

	root := NewFS("test.db", "auth_tokens.json", 5*time.Second)
	fsCache = root.GetCache()
	auth = fsCache.GetAuth()
	second := time.Second
	server, _ := fs.Mount(mountLoc, root, &fs.Options{
		EntryTimeout: &second,
		AttrTimeout:  &second,
		MountOptions: fuse.MountOptions{
			Name:          "onedriver",
			FsName:        "onedriver",
			DisableXAttrs: true,
			MaxBackground: 1024,
		},
	})

	// setup sigint handler for graceful unmount on interrupt/terminate
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go UnmountHandler(sigChan, server)

	// mount fs in background thread
	go server.Serve()

	// cleanup from last run
	log.Info("Setup test environment ---------------------------------")
	os.RemoveAll(TestDir)
	os.Mkdir(TestDir, 0755)
	os.Mkdir(DeltaDir, 0755)
	// we do not cd into the mounted directory or it will hang indefinitely on
	// unmount with "device or resource busy"

	log.Info("Test session start ---------------------------------")

	// run tests
	code := m.Run()

	log.Info("Test session end -----------------------------------")
	fmt.Printf("Waiting 5 seconds for any remaining uploads to complete")
	for i := 0; i < 5; i++ {
		time.Sleep(time.Second)
		fmt.Printf(".")
	}
	fmt.Printf("\n")

	// unmount
	if server.Unmount() != nil {
		log.Error("Failed to unmount test fuse server, attempting lazy unmount")
		exec.Command("fusermount", "-zu", "mount").Run()
	}
	fmt.Println("Successfully unmounted fuse server!")
	os.Exit(code)
}

// convenience handler to fail tests if an error is not nil
func failOnErr(t *testing.T, err error) {
	if err != nil {
		_, file, line, _ := runtime.Caller(1)
		t.Logf("Test failed at %s:%d:\n", file, line)
		t.Fatal(err)
	}
}
