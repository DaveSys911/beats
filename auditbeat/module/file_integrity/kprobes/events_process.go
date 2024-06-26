// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

//go:build linux

package kprobes

import (
	"context"
	"errors"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type Emitter interface {
	Emit(ePath string, pid uint32, op uint32) error
}

type eProcessor struct {
	p           pathTraverser
	e           Emitter
	d           *dEntryCache
	isRecursive bool
}

func newEventProcessor(p pathTraverser, e Emitter, isRecursive bool) *eProcessor {
	return &eProcessor{
		p:           p,
		e:           e,
		d:           newDirEntryCache(),
		isRecursive: isRecursive,
	}
}

func (e *eProcessor) process(_ context.Context, pe *ProbeEvent) error {
	// after processing return the probe event to the pool
	defer releaseProbeEvent(pe)

	switch {
	case pe.MaskMonitor == 1:
		// Monitor events are only generated by our own pathTraverser.AddPathToMonitor or
		// pathTraverser.WalkAsync

		monitorPath, match := e.p.GetMonitorPath(pe.FileIno, pe.FileDevMajor, pe.FileDevMinor, pe.FileName)
		if !match {
			return nil
		}

		entry := e.d.Get(dKey{
			Ino:      pe.FileIno,
			DevMajor: pe.FileDevMajor,
			DevMinor: pe.FileDevMinor,
		})

		parentEntry := e.d.Get(dKey{
			Ino:      pe.ParentIno,
			DevMajor: pe.ParentDevMajor,
			DevMinor: pe.ParentDevMinor,
		})

		if parentEntry == nil {
			entry = &dEntry{
				Name:     monitorPath.fullPath,
				Ino:      pe.FileIno,
				Depth:    monitorPath.depth,
				DevMajor: pe.FileDevMajor,
				DevMinor: pe.FileDevMinor,
			}
		} else {
			if entry == nil {
				entry = &dEntry{
					Name:     pe.FileName,
					Ino:      pe.FileIno,
					Depth:    parentEntry.Depth + 1,
					DevMajor: pe.FileDevMajor,
					DevMinor: pe.FileDevMinor,
				}
			}
		}

		e.d.Add(entry, parentEntry)

		if !monitorPath.isFromMove {
			return nil
		}

		return e.e.Emit(entry.Path(), monitorPath.tid, unix.IN_MOVED_TO)

	case pe.MaskCreate == 1:
		parentEntry := e.d.Get(dKey{
			Ino:      pe.ParentIno,
			DevMajor: pe.ParentDevMajor,
			DevMinor: pe.ParentDevMinor,
		})

		if parentEntry == nil || parentEntry.Depth >= 1 && !e.isRecursive {
			return nil
		}

		entry := &dEntry{
			Children: nil,
			Name:     pe.FileName,
			Ino:      pe.FileIno,
			DevMajor: pe.FileDevMajor,
			DevMinor: pe.FileDevMinor,
		}

		e.d.Add(entry, parentEntry)

		return e.e.Emit(entry.Path(), pe.Meta.TID, unix.IN_CREATE)

	case pe.MaskModify == 1:
		entry := e.d.Get(dKey{
			Ino:      pe.FileIno,
			DevMajor: pe.FileDevMajor,
			DevMinor: pe.FileDevMinor,
		})

		if entry == nil {
			return nil
		}

		return e.e.Emit(entry.Path(), pe.Meta.TID, unix.IN_MODIFY)

	case pe.MaskAttrib == 1:
		entry := e.d.Get(dKey{
			Ino:      pe.FileIno,
			DevMajor: pe.FileDevMajor,
			DevMinor: pe.FileDevMinor,
		})

		if entry == nil {
			return nil
		}

		return e.e.Emit(entry.Path(), pe.Meta.TID, unix.IN_ATTRIB)

	case pe.MaskMoveFrom == 1:
		parentEntry := e.d.Get(dKey{
			Ino:      pe.ParentIno,
			DevMajor: pe.ParentDevMajor,
			DevMinor: pe.ParentDevMinor,
		})

		if parentEntry == nil || parentEntry.Depth >= 1 && !e.isRecursive {
			e.d.MoveClear(uint64(pe.Meta.TID))
			return nil
		}

		entry := parentEntry.GetChild(pe.FileName)
		if entry == nil {
			return nil
		}

		entryPath := entry.Path()

		e.d.MoveFrom(uint64(pe.Meta.TID), entry)

		return e.e.Emit(entryPath, pe.Meta.TID, unix.IN_MOVED_FROM)

	case pe.MaskMoveTo == 1:
		parentEntry := e.d.Get(dKey{
			Ino:      pe.ParentIno,
			DevMajor: pe.ParentDevMajor,
			DevMinor: pe.ParentDevMinor,
		})

		if parentEntry == nil || parentEntry.Depth >= 1 && !e.isRecursive {
			// if parentEntry is nil then this move event is not
			// for a directory we monitor
			e.d.MoveClear(uint64(pe.Meta.TID))
			return nil
		}

		if existingChild := parentEntry.GetChild(pe.FileName); existingChild != nil {
			e.d.Remove(existingChild)
			existingChild.Release()
		}

		moved, err := e.d.MoveTo(uint64(pe.Meta.TID), parentEntry, pe.FileName, func(path string) error {
			return e.e.Emit(path, pe.Meta.TID, unix.IN_MOVED_TO)
		})
		if err != nil {
			return err
		}
		if moved {
			return nil
		}

		newEntryPath := filepath.Join(parentEntry.Path(), pe.FileName)
		e.p.WalkAsync(newEntryPath, parentEntry.Depth+1, pe.Meta.TID)

		return nil

	case pe.MaskDelete == 1:
		parentEntry := e.d.Get(dKey{
			Ino:      pe.ParentIno,
			DevMajor: pe.ParentDevMajor,
			DevMinor: pe.ParentDevMinor,
		})

		if parentEntry == nil || parentEntry.Depth >= 1 && !e.isRecursive {
			return nil
		}

		entry := parentEntry.GetChild(pe.FileName)
		if entry == nil {
			return nil
		}

		entryPath := entry.Path()

		e.d.Remove(entry)

		if err := e.e.Emit(entryPath, pe.Meta.TID, unix.IN_DELETE); err != nil {
			return err
		}

		entry.Release()

		return nil
	default:
		return errors.New("unknown event type")
	}
}
