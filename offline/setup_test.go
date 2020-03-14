package offline

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	odfs "github.com/jstaf/onedriver/fs"
	"github.com/jstaf/onedriver/fs/graph"
	"github.com/jstaf/onedriver/logger"
	log "github.com/sirupsen/logrus"
)

const (
	mountLoc = "mount"
	TestDir  = mountLoc + "/onedriver_tests"
)

var auth *graph.Auth

// Like the graph package, but designed for running tests offline.
func TestMain(m *testing.M) {
	//os.Chdir("..")
	// attempt to unmount regardless of what happens (in case previous tests
	// failed and didn't clean themselves up)
	exec.Command("fusermount", "-uz", mountLoc).Run()
	os.Mkdir(mountLoc, 0755)

	auth := graph.Authenticate(".auth_tokens.json")
	inode, err := graph.GetItem("root", auth)
	if inode != nil || !graph.IsOffline(err) {
		fmt.Println("These tests must be run offline.")
		os.Exit(1)
	}

	f := logger.LogTestSetup()
	defer f.Close()
	log.Info("Setup offline tests ------------------------------")

	// reuses the cached data from the previous tests
	root := odfs.NewFS("test.db", ".auth_tokens.json", 5*time.Second)
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
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGABRT)
	go odfs.UnmountHandler(sigChan, server)

	// mount fs in background thread
	go server.Serve()

	log.Info("Start offline tests ------------------------------")
	code := m.Run()
	log.Info("Finish offline tests ------------------------------")

	if server.Unmount() != nil {
		log.Error("Failed to unmount test fuse server, attempting lazy unmount")
		exec.Command("fusermount", "-zu", "mount").Run()
	}
	fmt.Println("Successfully unmounted fuse server!")
	os.Exit(code)
}
