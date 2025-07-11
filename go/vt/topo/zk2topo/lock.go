/*
Copyright 2019 The Vitess Authors.

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

package zk2topo

import (
	"context"
	"fmt"
	"path"
	"time"

	"github.com/z-division/go-zookeeper/zk"

	"vitess.io/vitess/go/vt/vterrors"

	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/topo"
)

// This file contains the lock management code for zktopo.Server.

// zkLockDescriptor implements topo.LockDescriptor.
type zkLockDescriptor struct {
	zs       *Server
	nodePath string
}

// Lock is part of the topo.Conn interface.
func (zs *Server) Lock(ctx context.Context, dirPath, contents string) (topo.LockDescriptor, error) {
	return zs.lock(ctx, dirPath, contents)
}

// LockWithTTL is part of the topo.Conn interface. It behaves the same as Lock
// as TTLs are not supported in Zookeeper.
func (zs *Server) LockWithTTL(ctx context.Context, dirPath, contents string, _ time.Duration) (topo.LockDescriptor, error) {
	return zs.lock(ctx, dirPath, contents)
}

// LockName is part of the topo.Conn interface.
func (zs *Server) LockName(ctx context.Context, dirPath, contents string) (topo.LockDescriptor, error) {
	return zs.lock(ctx, dirPath, contents)
}

// TryLock is part of the topo.Conn interface.
func (zs *Server) TryLock(ctx context.Context, dirPath, contents string) (topo.LockDescriptor, error) {
	// We list all the entries under dirPath
	entries, err := zs.ListDir(ctx, dirPath, true)
	if err != nil {
		// We need to return the right error codes, like
		// topo.ErrNoNode and topo.ErrInterrupted, and the
		// easiest way to do this is to return convertError(err).
		// It may lose some of the context, if this is an issue,
		// maybe logging the error would work here.
		return nil, convertError(err, dirPath)
	}

	// If there is a folder '/locks' with some entries in it then we can assume that someone else already has a lock.
	// Throw error in this case
	for _, e := range entries {
		// there is a bug where ListDir return ephemeral = false for locks. It is due
		// https://github.com/vitessio/vitess/blob/main/go/vt/topo/zk2topo/utils.go#L55
		// TODO: Fix/send ephemeral flag value recursively while creating ephemeral file
		if e.Name == locksPath && e.Type == topo.TypeDirectory {
			return nil, topo.NewError(topo.NodeExists, fmt.Sprintf("lock already exists at path %s", dirPath))
		}
	}

	// everything is good let's acquire the lock.
	return zs.lock(ctx, dirPath, contents)
}

// Lock is part of the topo.Conn interface.
func (zs *Server) lock(ctx context.Context, dirPath, contents string) (topo.LockDescriptor, error) {
	// Lock paths end in a trailing slash so that when we create
	// sequential nodes, they are created as children, not siblings.
	locksDir := path.Join(zs.root, dirPath, locksPath) + "/"

	// Create the lock path, creating the parents as needed.
	nodePath, err := CreateRecursive(ctx, zs.conn, locksDir, []byte(contents), zk.FlagSequence|zk.FlagEphemeral, zk.WorldACL(PermFile), -1)
	if err != nil {
		return nil, convertError(err, locksDir)
	}

	err = obtainQueueLock(ctx, zs.conn, nodePath)
	if err != nil {
		var errToReturn error
		switch err {
		case context.DeadlineExceeded:
			errToReturn = topo.NewError(topo.Timeout, nodePath)
		case context.Canceled:
			errToReturn = topo.NewError(topo.Interrupted, nodePath)
		default:
			errToReturn = vterrors.Wrapf(err, "failed to obtain lock: %v", nodePath)
		}

		// Regardless of the reason, try to cleanup.
		log.Warningf("Failed to obtain lock: %v", err)

		cleanupCtx, cancel := context.WithTimeout(context.Background(), baseTimeout)
		defer cancel()

		if err := zs.conn.Delete(cleanupCtx, nodePath, -1); err != nil {
			log.Warningf("Failed to cleanup unsuccessful lock path %s: %v", nodePath, err)
		}

		// Show the other locks in the directory
		dir := path.Dir(nodePath)
		children, _, err := zs.conn.Children(cleanupCtx, dir)
		if err != nil {
			log.Warningf("Failed to get children of %v: %v", dir, err)
			return nil, errToReturn
		}

		if len(children) == 0 {
			log.Warningf("No other locks present, you may just try again now.")
			return nil, errToReturn
		}

		childPath := path.Join(dir, children[0])
		data, _, err := zs.conn.Get(cleanupCtx, childPath)
		if err != nil {
			log.Warningf("Failed to get first locks node %v (may have just ended): %v", childPath, err)
			return nil, errToReturn
		}

		log.Warningf("------ Most likely blocking lock: %v\n%v", childPath, string(data))
		return nil, errToReturn
	}

	// Remove the root prefix from the file. So when we delete it,
	// it's a relative file.
	nodePath = nodePath[len(zs.root):]
	return &zkLockDescriptor{
		zs:       zs,
		nodePath: nodePath,
	}, nil
}

// Check is part of the topo.LockDescriptor interface.
func (ld *zkLockDescriptor) Check(ctx context.Context) error {
	// TODO(alainjobart): check the connection has not been interrupted.
	// We'd lose the ephemeral node in case of a session loss.
	return nil
}

// Unlock is part of the topo.LockDescriptor interface.
func (ld *zkLockDescriptor) Unlock(ctx context.Context) error {
	return ld.zs.Delete(ctx, ld.nodePath, nil)
}
