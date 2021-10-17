package fs

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/jstaf/onedriver/fs/graph"
	log "github.com/sirupsen/logrus"
)

const timeout = time.Second

// getInodeContent returns a copy of the inode's content. Ensures that data is non-nil.
func (f *Filesystem) getInodeContent(i *Inode) *[]byte {
	i.RLock()
	defer i.RUnlock()

	if i.data != nil {
		data := make([]byte, i.DriveItem.Size)
		copy(data, *i.data)
		return &data
	}
	data := f.GetContent(i.DriveItem.ID)
	return &data
}

// remoteID uploads a file to obtain a Onedrive ID if it doesn't already
// have one. This is necessary to avoid race conditions against uploads if the
// file has not already been uploaded.
func (f *Filesystem) remoteID(i *Inode) (string, error) {
	if i.IsDir() {
		// Directories are always created with an ID. (And this method is only
		// really used for files anyways...)
		return i.ID(), nil
	}

	originalID := i.ID()
	if isLocalID(originalID) && f.auth.AccessToken != "" {
		// perform a blocking upload of the item
		data := f.getInodeContent(i)
		session, err := NewUploadSession(i, data)
		if err != nil {
			return originalID, err
		}

		i.Lock()
		name := i.DriveItem.Name
		err = session.Upload(f.auth)
		if err != nil {
			i.Unlock()

			if strings.Contains(err.Error(), "nameAlreadyExists") {
				// A file with this name already exists on the server, get its ID and
				// use that. This is probably the same file, but just got uploaded
				// earlier.
				children, err := graph.GetItemChildren(i.ParentID(), f.auth)
				if err != nil {
					return originalID, err
				}
				for _, child := range children {
					if child.Name == name {
						log.WithFields(log.Fields{
							"name":     name,
							"original": originalID,
							"new":      child.ID,
						}).Info("Exchanged ID.")
						return child.ID, f.MoveID(originalID, child.ID)
					}
				}
			}
			// failed to obtain an ID, return whatever it was beforehand
			return originalID, err
		}

		// we just successfully uploaded a copy, no need to do it again
		i.hasChanges = false
		i.DriveItem.ETag = session.ETag
		i.Unlock()

		// this is all we really wanted from this transaction
		err = f.MoveID(originalID, session.ID)
		log.WithFields(log.Fields{
			"name":     name,
			"original": originalID,
			"new":      session.ID,
		}).Info("Exchanged ID.")
		return session.ID, err
	}
	return originalID, nil
}

// Statfs returns information about the filesystem. Mainly useful for checking
// quotas and storage limits.
func (f *Filesystem) StatFs(cancel <-chan struct{}, in *fuse.InHeader, out *fuse.StatfsOut) fuse.Status {
	log.Debug("Statfs")
	drive, err := graph.GetDrive(f.auth)
	if err != nil {
		return fuse.EREMOTEIO
	}

	if drive.DriveType == graph.DriveTypePersonal {
		log.Warn("Personal OneDrive accounts do not show number of files, " +
			"inode counts reported by onedriver will be bogus.")
	} else if drive.Quota.Total == 0 { // <-- check for if microsoft ever fixes their API
		log.Warn("OneDrive for Business accounts do not report quotas, " +
			"pretending the quota is 5TB and it's all unused.")
		drive.Quota.Total = 5 * uint64(math.Pow(1024, 4))
		drive.Quota.Remaining = 5 * uint64(math.Pow(1024, 4))
		drive.Quota.FileCount = 0
	}

	// limits are pasted from https://support.microsoft.com/en-us/help/3125202
	const blkSize uint64 = 4096 // default ext4 block size
	out.Bsize = uint32(blkSize)
	out.Blocks = drive.Quota.Total / blkSize
	out.Bfree = drive.Quota.Remaining / blkSize
	out.Bavail = drive.Quota.Remaining / blkSize
	out.Files = 100000
	out.Ffree = 100000 - drive.Quota.FileCount
	out.NameLen = 260
	return fuse.OK
}

// Mkdir creates a directory.
func (f *Filesystem) Mkdir(cancel <-chan struct{}, in *fuse.MkdirIn, name string, out *fuse.EntryOut) fuse.Status {
	inode := f.GetNodeID(in.NodeId)
	if inode == nil {
		return fuse.ENOENT
	}
	id := inode.ID()
	path := filepath.Join(inode.Path(), name)
	log.WithFields(log.Fields{
		"nodeID": in.NodeId,
		"id":     id,
		"path":   path,
		"mode":   in.Mode,
	}).Debug()

	// create the new directory on the server
	item, err := graph.Mkdir(name, id, f.auth)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"nodeID": in.NodeId,
			"id":     id,
			"path":   path,
		}).Error("Error during remote directory creation.")
		return fuse.EREMOTEIO
	}

	newInode := NewInodeDriveItem(item)
	newInode.mode = in.Mode | fuse.S_IFDIR

	out.NodeId = f.InsertChild(id, newInode)
	out.Attr = newInode.makeAttr()
	out.SetAttrTimeout(timeout)
	out.SetEntryTimeout(timeout)
	return fuse.OK
}

// Rmdir removes a directory if it's empty.
func (f *Filesystem) Rmdir(cancel <-chan struct{}, in *fuse.InHeader, name string) fuse.Status {
	parentID := f.TranslateID(in.NodeId)
	if parentID == "" {
		return fuse.ENOENT
	}
	child, _ := f.GetChild(parentID, name, f.auth)
	if child == nil {
		return fuse.ENOENT
	}
	if child.HasChildren() {
		return fuse.Status(syscall.ENOTEMPTY)
	}
	return f.Unlink(cancel, in, name)
}

// ReadDir provides a list of all the entries in the directory
func (f *Filesystem) OpenDir(cancel <-chan struct{}, in *fuse.OpenIn, out *fuse.OpenOut) fuse.Status {
	id := f.TranslateID(in.NodeId)
	dir := f.GetID(id)
	if dir == nil {
		return fuse.ENOENT
	}
	if !dir.IsDir() {
		return fuse.ENOTDIR
	}
	path := dir.Path()
	log.WithFields(log.Fields{
		"nodeID": in.NodeId,
		"id":     id,
		"path":   path,
	}).Debug()

	children, err := f.GetChildrenID(id, f.auth)
	if err != nil {
		// not an item not found error (Lookup/Getattr will always be called
		// before Readdir()), something has happened to our connection
		log.WithError(err).WithFields(log.Fields{
			"nodeID": in.NodeId,
			"id":     id,
			"path":   path,
		}).Error("Could not fetch children")
		return fuse.EREMOTEIO
	}

	parent := f.GetID(dir.ParentID())
	if parent == nil {
		// This is the parent of the mountpoint. The FUSE kernel module discards
		// this info, so what we put here doesn't actually matter.
		parent = NewInode("..", 0755|fuse.S_IFDIR, nil)
		parent.nodeID = math.MaxUint64
	}

	entries := make([]*Inode, 2)
	entries[0] = dir
	entries[1] = parent

	for _, child := range children {
		entries = append(entries, child)
	}
	f.opendirsM.Lock()
	f.opendirs[in.NodeId] = entries
	f.opendirsM.Unlock()

	return fuse.OK
}

// ReleaseDir closes a directory and purges it from memory
func (f *Filesystem) ReleaseDir(in *fuse.ReleaseIn) {
	f.opendirsM.Lock()
	delete(f.opendirs, in.NodeId)
	f.opendirsM.Unlock()
}

// ReadDirPlus reads an individual directory entry AND does a lookup.
func (f *Filesystem) ReadDirPlus(cancel <-chan struct{}, in *fuse.ReadIn, out *fuse.DirEntryList) fuse.Status {
	f.opendirsM.RLock()
	entries, ok := f.opendirs[in.NodeId]
	f.opendirsM.RUnlock()
	if !ok {
		// readdir can sometimes arrive before the corresponding opendir, so we force it
		f.OpenDir(cancel, &fuse.OpenIn{InHeader: in.InHeader}, nil)
		f.opendirsM.RLock()
		entries, ok = f.opendirs[in.NodeId]
		f.opendirsM.RUnlock()
		if !ok {
			return fuse.EBADF
		}
	}

	if in.Offset >= uint64(len(entries)) {
		// just tried to seek past end of directory, we're all done!
		return fuse.OK
	}

	inode := entries[in.Offset]
	entry := fuse.DirEntry{
		Ino:  inode.NodeID(),
		Mode: inode.Mode(),
	}
	// first two entries will always be "." and ".."
	switch in.Offset {
	case 0:
		entry.Name = "."
	case 1:
		entry.Name = ".."
	default:
		entry.Name = inode.Name()
	}
	entryOut := out.AddDirLookupEntry(entry)
	if entryOut == nil {
		//FIXME probably need to handle this better using the "overflow stuff"
		log.WithFields(log.Fields{
			"nodeID":      in.NodeId,
			"offset":      in.Offset,
			"entryName":   entry.Name,
			"entryNodeID": entry.Ino,
		}).Error("Exceeded DirLookupEntry bounds!")
		return fuse.EIO
	}
	entryOut.NodeId = entry.Ino
	entryOut.Attr = inode.makeAttr()
	entryOut.SetAttrTimeout(timeout)
	entryOut.SetEntryTimeout(timeout)
	return fuse.OK
}

// ReadDir reads a directory entry. Usually doesn't get called (ReadDirPlus is
// typically used).
func (f *Filesystem) ReadDir(cancel <-chan struct{}, in *fuse.ReadIn, out *fuse.DirEntryList) fuse.Status {
	f.opendirsM.RLock()
	entries, ok := f.opendirs[in.NodeId]
	f.opendirsM.RUnlock()
	if !ok {
		// readdir can sometimes arrive before the corresponding opendir, so we force it
		f.OpenDir(cancel, &fuse.OpenIn{InHeader: in.InHeader}, nil)
		f.opendirsM.RLock()
		entries, ok = f.opendirs[in.NodeId]
		f.opendirsM.RUnlock()
		if !ok {
			return fuse.EBADF
		}
	}

	if in.Offset >= uint64(len(entries)) {
		// just tried to seek past end of directory, we're all done!
		return fuse.OK
	}

	inode := entries[in.Offset]
	entry := fuse.DirEntry{
		Ino:  inode.NodeID(),
		Mode: inode.Mode(),
	}
	// first two entries will always be "." and ".."
	switch in.Offset {
	case 0:
		entry.Name = "."
	case 1:
		entry.Name = ".."
	default:
		entry.Name = inode.Name()
	}

	out.AddDirEntry(entry)
	return fuse.OK
}

// Lookup is called by the kernel when the VFS wants to know about a file inside
// a directory.
func (f *Filesystem) Lookup(cancel <-chan struct{}, in *fuse.InHeader, name string, out *fuse.EntryOut) fuse.Status {
	id := f.TranslateID(in.NodeId)
	log.WithFields(log.Fields{
		"nodeID": in.NodeId,
		"id":     id,
		"name":   name,
	}).Trace()

	child, _ := f.GetChild(id, strings.ToLower(name), f.auth)
	if child == nil {
		return fuse.ENOENT
	}

	out.NodeId = child.NodeID()
	out.Attr = child.makeAttr()
	out.SetAttrTimeout(timeout)
	out.SetEntryTimeout(timeout)
	return fuse.OK
}

// Mknod creates a regular file. The server doesn't have this yet.
func (f *Filesystem) Mknod(cancel <-chan struct{}, in *fuse.MknodIn, name string, out *fuse.EntryOut) fuse.Status {
	parentID := f.TranslateID(in.NodeId)
	if parentID == "" {
		return fuse.EBADF
	}

	parent := f.GetID(parentID)
	if parent == nil {
		return fuse.ENOENT
	}

	path := filepath.Join(parent.Path(), name)
	if f.IsOffline() {
		log.WithFields(log.Fields{
			"id":     parentID,
			"nodeID": in.NodeId,
			"path":   path,
		}).Warn("We are offline. Refusing Mknod() to avoid data loss later.")
		return fuse.EROFS
	}

	if child, _ := f.GetChild(parentID, name, f.auth); child != nil {
		return fuse.Status(syscall.EEXIST)
	}

	inode := NewInode(name, in.Mode, parent)
	log.WithFields(log.Fields{
		"id":      parentID,
		"childID": inode.ID(),
		"path":    path,
		"mode":    Octal(in.Mode),
	}).Debug("Creating inode.")
	out.NodeId = f.InsertChild(parentID, inode)
	out.Attr = inode.makeAttr()
	out.SetAttrTimeout(timeout)
	out.SetEntryTimeout(timeout)
	return fuse.OK
}

// Create creates a regular file and opens it. The server doesn't have this yet.
func (f *Filesystem) Create(cancel <-chan struct{}, in *fuse.CreateIn, name string, out *fuse.CreateOut) fuse.Status {
	// we reuse mknod here
	result := f.Mknod(
		cancel,
		// we don't actually use the umask or padding here, so they don't get passed
		&fuse.MknodIn{
			InHeader: in.InHeader,
			Mode:     in.Mode,
		},
		name,
		&out.EntryOut,
	)
	if result == fuse.Status(syscall.EEXIST) {
		// if the inode already exists, we should truncate the existing file and
		// return the existing file inode as per "man creat"
		parentID := f.TranslateID(in.NodeId)
		child, _ := f.GetChild(parentID, name, f.auth)
		log.WithFields(log.Fields{
			"id":      parentID,
			"childID": child.ID(),
			"path":    child.Path(),
			"mode":    Octal(in.Mode),
		}).Debug("Child inode already exists, truncating.")
		child.data = nil
		child.DriveItem.Size = 0
		child.hasChanges = true
		return fuse.OK
	}
	// no further initialized required to open the file, it's empty
	return result
}

// Open fetches a Inodes's content and initializes the .Data field with actual
// data from the server. Data is loaded into memory on Open, and persisted to
// disk on Flush.
func (f *Filesystem) Open(cancel <-chan struct{}, in *fuse.OpenIn, out *fuse.OpenOut) fuse.Status {
	id := f.TranslateID(in.NodeId)
	inode := f.GetID(id)
	if inode == nil {
		return fuse.ENOENT
	}

	path := inode.Path()
	flags := int(in.Flags)
	if flags&os.O_RDWR+flags&os.O_WRONLY > 0 && f.IsOffline() {
		log.WithFields(log.Fields{
			"nodeID":    in.NodeId,
			"id":        id,
			"path":      path,
			"readWrite": bool(flags&os.O_RDWR > 0),
			"writeOnly": bool(flags&os.O_WRONLY > 0),
		}).Debug("Refusing Open() with write flag, FS is offline.")
		return fuse.EROFS
	}

	log.WithFields(log.Fields{
		"nodeID": in.NodeId,
		"id":     id,
		"path":   path,
	}).Debug("Opening file for I/O.")

	if inode.HasContent() {
		// we already have data, likely the file is already opened somewhere
		return fuse.OK
	}

	// try grabbing from disk
	if content := f.GetContent(id); content != nil {
		// verify content against what we're supposed to have
		var hashMatch bool
		inode.RLock()
		driveType := inode.DriveItem.Parent.DriveType
		if isLocalID(id) && inode.DriveItem.File == nil {
			// only check hashes if the file has been uploaded before, otherwise
			// we just accept the cached content.
			hashMatch = true
		} else if driveType == graph.DriveTypePersonal {
			hashMatch = inode.VerifyChecksum(graph.SHA1Hash(&content))
		} else if driveType == graph.DriveTypeBusiness || driveType == graph.DriveTypeSharepoint {
			hashMatch = inode.VerifyChecksum(graph.QuickXORHash(&content))
		} else {
			hashMatch = true
			log.WithFields(log.Fields{
				"driveType": driveType,
				"nodeID":    in.NodeId,
				"id":        id,
				"path":      path,
			}).Warn("Could not determine drive type, not checking hashes.")
		}
		inode.RUnlock()

		if hashMatch {
			// disk content is only used if the checksums match
			log.WithFields(log.Fields{
				"id":     id,
				"nodeID": in.NodeId,
				"path":   path,
			}).Info("Found content in cache.")

			inode.Lock()
			defer inode.Unlock()
			// this check is here in case the API file sizes are WRONG (it happens)
			inode.DriveItem.Size = uint64(len(content))
			inode.data = &content
			return fuse.OK
		}
		log.WithFields(log.Fields{
			"id":        id,
			"nodeID":    in.NodeId,
			"path":      path,
			"drivetype": driveType,
		}).Info("Not using cached item due to file hash mismatch.")
	}

	if isLocalID(id) {
		log.WithFields(log.Fields{
			"id":     id,
			"nodeID": in.NodeId,
			"path":   path,
		}).Error("Item has a local ID, and we failed to find the cached local content!")
		return fuse.ENODATA
	}

	// didn't have it on disk, now try api
	log.WithFields(log.Fields{
		"id":     id,
		"nodeID": in.NodeId,
		"path":   path,
	}).Info("Fetching remote content for item from API.")

	body, err := graph.GetItemContent(id, f.auth)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"path":   path,
			"id":     id,
			"nodeID": in.NodeId,
		}).Error("Failed to fetch remote content.")
		return fuse.EREMOTEIO
	}

	inode.Lock()
	defer inode.Unlock()
	// this check is here in case the API file sizes are WRONG (it happens)
	inode.DriveItem.Size = uint64(len(body))
	inode.data = &body
	return fuse.OK
}

// Unlink deletes a child file.
func (f *Filesystem) Unlink(cancel <-chan struct{}, in *fuse.InHeader, name string) fuse.Status {
	parentID := f.TranslateID(in.NodeId)
	child, _ := f.GetChild(parentID, name, nil)
	if child == nil {
		// the file we are unlinking never existed
		return fuse.ENOENT
	}
	if f.IsOffline() {
		return fuse.EROFS
	}

	id := child.ID()
	path := child.Path()
	log.WithFields(log.Fields{
		"nodeID":  in.NodeId,
		"id":      parentID,
		"childID": id,
		"path":    path,
	}).Debug("Unlinking inode.")

	// if no ID, the item is local-only, and does not need to be deleted on the
	// server
	if !isLocalID(id) {
		if err := graph.Remove(id, f.auth); err != nil {
			log.WithError(err).WithFields(log.Fields{
				"nodeID":   in.NodeId,
				"path":     path,
				"id":       id,
				"parentID": parentID,
			}).Error("Failed to delete item on server. Aborting op.")
			return fuse.EREMOTEIO
		}
	}

	f.DeleteID(id)
	f.DeleteContent(id)
	return fuse.OK
}

// Read an inode's data like a file.
func (f *Filesystem) Read(cancel <-chan struct{}, in *fuse.ReadIn, buf []byte) (fuse.ReadResult, fuse.Status) {
	inode := f.GetNodeID(in.NodeId)
	if inode == nil {
		return fuse.ReadResultData(make([]byte, 0)), fuse.EBADF
	}

	path := inode.Path()
	if !inode.HasContent() {
		log.WithFields(log.Fields{
			"nodeID": in.NodeId,
			"id":     inode.ID(),
			"path":   path,
		}).Warn("Read called on a closed file descriptor! Reopening file for op.")
		f.Open(cancel, &fuse.OpenIn{InHeader: in.InHeader}, &fuse.OpenOut{})
	}

	// we are locked for the remainder of this op
	inode.RLock()
	defer inode.RUnlock()
	if inode.data == nil {
		// file got flushed somehow in between here and when this function was called
		return fuse.ReadResultData(make([]byte, 0)), fuse.EAGAIN
	}

	off := in.Offset
	end := int(off) + int(len(buf))
	oend := end
	size := len(*inode.data) // worse than using i.Size(), but some edge cases require it
	if int(off) > size {
		log.WithFields(log.Fields{
			"id":        inode.DriveItem.ID,
			"nodeID":    in.NodeId,
			"path":      path,
			"bufsize":   uint64(end) - off,
			"file_size": size,
			"offset":    off,
		}).Error("Offset was beyond file end (Onedrive metadata was wrong!). Refusing op.")
		return fuse.ReadResultData(make([]byte, 0)), fuse.EINVAL
	}
	if end > size {
		end = size
	}
	log.WithFields(log.Fields{
		"id":               inode.DriveItem.ID,
		"nodeID":           in.NodeId,
		"path":             path,
		"original_bufsize": uint64(oend) - off,
		"bufsize":          uint64(end) - off,
		"file_size":        size,
		"offset":           off,
	}).Trace("Read file")
	return fuse.ReadResultData((*inode.data)[off:end]), 0
}

// Write to an Inode like a file. Note that changes are 100% local until
// Flush() is called. Returns the number of bytes written and the status of the
// op.
func (f *Filesystem) Write(cancel <-chan struct{}, in *fuse.WriteIn, data []byte) (uint32, fuse.Status) {
	id := f.TranslateID(in.NodeId)
	inode := f.GetID(id)
	if inode == nil {
		return 0, fuse.EBADF
	}

	nWrite := len(data)
	offset := int(in.Offset)
	log.WithFields(log.Fields{
		"id":      id,
		"nodeID":  in.NodeId,
		"path":    inode.Path(),
		"bufsize": nWrite,
		"offset":  offset,
	}).Trace("Write file")

	if !inode.HasContent() {
		log.WithFields(log.Fields{
			"id":     id,
			"nodeID": in.NodeId,
			"path":   inode.Path(),
		}).Warn("Write called on a closed file descriptor! Reopening file for write op.")
		f.Open(cancel, &fuse.OpenIn{InHeader: in.InHeader, Flags: in.WriteFlags}, &fuse.OpenOut{})
	}

	inode.Lock()
	defer inode.Unlock()
	if offset+nWrite > int(inode.DriveItem.Size)-1 {
		// we've exceeded the file size, overwrite via append
		*inode.data = append((*inode.data)[:offset], data...)
	} else {
		// writing inside the current file, overwrite in place
		copy((*inode.data)[offset:], data)
	}
	// probably a better way to do this, but whatever
	inode.DriveItem.Size = uint64(len(*inode.data))
	inode.hasChanges = true
	return uint32(nWrite), fuse.OK
}

// Fsync is a signal to ensure writes to the Inode are flushed to stable
// storage. This method is used to trigger uploads of file content.
func (f *Filesystem) Fsync(cancel <-chan struct{}, in *fuse.FsyncIn) fuse.Status {
	id := f.TranslateID(in.NodeId)
	inode := f.GetID(id)
	if inode == nil {
		return fuse.EBADF
	}

	path := inode.Path()
	log.WithFields(log.Fields{
		"id":   id,
		"path": path,
	}).Debug()
	if inode.HasChanges() {
		inode.Lock()
		inode.hasChanges = false

		// recompute hashes when saving new content
		inode.DriveItem.File = &graph.File{}
		if inode.DriveItem.Parent.DriveType == graph.DriveTypePersonal {
			inode.DriveItem.File.Hashes.SHA1Hash = graph.SHA1Hash(inode.data)
		} else {
			inode.DriveItem.File.Hashes.QuickXorHash = graph.QuickXORHash(inode.data)
		}
		inode.Unlock()

		if err := f.uploads.QueueUpload(inode); err != nil {
			log.WithFields(log.Fields{
				"id":   id,
				"path": path,
				"err":  err,
			}).Error("Error creating upload session.")
			return fuse.EREMOTEIO
		}
		return fuse.OK
	}
	return fuse.OK
}

// Flush is called when a file descriptor is closed. Uses Fsync() to perform file
// uploads. (Release not implemented because all cleanup is already done here).
func (f *Filesystem) Flush(cancel <-chan struct{}, in *fuse.FlushIn) fuse.Status {
	inode := f.GetNodeID(in.NodeId)
	if inode == nil {
		return fuse.EBADF
	}

	log.WithFields(log.Fields{
		"path":   inode.Path(),
		"nodeID": in.NodeId,
		"id":     inode.ID(),
	}).Debug()
	f.Fsync(cancel, &fuse.FsyncIn{InHeader: in.InHeader})

	// wipe data from memory to avoid mem bloat over time
	inode.Lock()
	if inode.data != nil {
		f.InsertContent(inode.DriveItem.ID, *inode.data)
		inode.data = nil
	}
	inode.Unlock()
	return 0
}

// Getattr returns a the Inode as a UNIX stat. Holds the read mutex for all of
// the "metadata fetch" operations.
func (f *Filesystem) GetAttr(cancel <-chan struct{}, in *fuse.GetAttrIn, out *fuse.AttrOut) fuse.Status {
	id := f.TranslateID(in.NodeId)
	inode := f.GetID(id)
	if inode == nil {
		return fuse.ENOENT
	}
	log.WithFields(log.Fields{
		"nodeID": in.NodeId,
		"id":     id,
		"path":   inode.Path(),
	}).Trace()

	out.Attr = inode.makeAttr()
	out.SetTimeout(timeout)
	return fuse.OK
}

// Setattr is the workhorse for setting filesystem attributes. Does the work of
// operations like utimens, chmod, chown (not implemented, FUSE is single-user),
// and truncate.
func (f *Filesystem) SetAttr(cancel <-chan struct{}, in *fuse.SetAttrIn, out *fuse.AttrOut) fuse.Status {
	i := f.GetNodeID(in.NodeId)
	if i == nil {
		return fuse.ENOENT
	}
	path := i.Path()
	isDir := i.IsDir() // holds an rlock
	i.Lock()
	log.WithFields(log.Fields{
		"nodeID": in.NodeId,
		"id":     i.DriveItem.ID,
		"path":   path,
	}).Debug()

	// utimens
	if mtime, valid := in.GetMTime(); valid {
		i.DriveItem.ModTime = &mtime
	}

	// chmod
	if mode, valid := in.GetMode(); valid {
		if isDir {
			i.mode = fuse.S_IFDIR | mode
		} else {
			i.mode = fuse.S_IFREG | mode
		}
	}

	// truncate
	if size, valid := in.GetSize(); valid {
		if size > i.DriveItem.Size {
			// unlikely to be hit, but implementing just in case
			extra := make([]byte, size-i.DriveItem.Size)
			*i.data = append(*i.data, extra...)
		} else {
			*i.data = (*i.data)[:size]
		}
		i.DriveItem.Size = size
		i.hasChanges = true
	}

	i.Unlock()
	out.Attr = i.makeAttr()
	out.SetTimeout(timeout)
	return fuse.OK
}

// Rename renames and/or moves an inode.
func (f *Filesystem) Rename(cancel <-chan struct{}, in *fuse.RenameIn, name string, newName string) fuse.Status {
	oldParentID := f.TranslateID(in.NodeId)
	oldParentItem := f.GetNodeID(in.NodeId)
	if oldParentID == "" || oldParentItem == nil {
		return fuse.EBADF
	}
	path := filepath.Join(oldParentItem.Path(), name)

	// we'll have the metadata for the dest inode already so it is not necessary
	// to use GetPath() to prefetch it. In order for the fs to know about this
	// inode, it has already fetched all of the inodes up to the new destination.
	newParentItem := f.GetNodeID(in.Newdir)
	if newParentItem == nil {
		return fuse.ENOENT
	}
	dest := filepath.Join(newParentItem.Path(), newName)

	// we don't fully trust DriveItem.Parent.Path from the Graph API
	log.WithFields(log.Fields{
		"srcNodeID": in.NodeId,
		"dstNodeID": in.Newdir,
		"path":      path,
		"dest":      dest,
	}).Debug("Renaming inode.")

	inode, _ := f.GetChild(oldParentID, name, f.auth)
	id, err := f.remoteID(inode)
	if isLocalID(id) || err != nil {
		// uploads will fail without an id
		log.WithFields(log.Fields{
			"id":   id,
			"path": path,
			"err":  err,
		}).Error("ID of item to move cannot be local and we failed to obtain an ID.")
		return fuse.EREMOTEIO
	}

	// perform remote rename
	newParentID := newParentItem.ID()
	if err = graph.Rename(id, newName, newParentID, f.auth); err != nil {
		log.WithFields(log.Fields{
			"nodeID":   in.NodeId,
			"id":       id,
			"parentID": newParentID,
			"path":     path,
			"dest":     dest,
			"err":      err,
		}).Error("Failed to rename remote item.")
		return fuse.EREMOTEIO
	}

	// now rename local copy
	if err = f.MovePath(oldParentID, newParentID, name, newName, f.auth); err != nil {
		log.WithFields(log.Fields{
			"nodeID": in.NodeId,
			"path":   path,
			"dest":   dest,
			"err":    err,
		}).Error("Failed to rename local item.")
		return fuse.EIO
	}

	// whew! item renamed
	return fuse.OK
}
