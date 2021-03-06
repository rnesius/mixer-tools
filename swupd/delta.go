package swupd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/clearlinux/mixer-tools/helpers"
	"github.com/pkg/errors"
)

const (
	// From swupd-server's include/swupd.h:
	//
	//     Approximately the smallest size of a pair of input files which differ by a
	//     single bit that bsdiff can produce a more compact deltafile. Files smaller
	//     than this are always marked as different. See the magic 200 value in the
	//     bsdiff/src/diff.c code.
	//
	minimumSizeToMakeDeltaInBytes = 200
)

// Delta represents a delta file between two other files. If Error is present, it
// indicates that the delta couldn't be created.
type Delta struct {
	Path  string
	Error error
	from  *File
	to    *File
}

// CreateDeltas creates all delta files between the previous and current version of the
// supplied manifest. Returns a list of deltas (which contains information about
// individual delta errors). Returns error (and no deltas) if it can't assemble the delta
// list. If number of workers is zero or less, 1 worker is used.
func CreateDeltas(manifest, statedir string, from, to uint32, numWorkers int) ([]Delta, error) {
	var c config

	c, err := getConfig(statedir)
	if err != nil {
		return nil, err
	}

	var oldManifest *Manifest
	var newManifest *Manifest

	if oldManifest, err = ParseManifestFile(filepath.Join(c.outputDir, fmt.Sprintf("%d", from), manifest)); err != nil {
		return nil, err
	}
	if newManifest, err = ParseManifestFile(filepath.Join(c.outputDir, fmt.Sprintf("%d", to), manifest)); err != nil {
		return nil, err
	}

	return createDeltasFromManifests(&c, oldManifest, newManifest, numWorkers)
}

func createDeltasFromManifests(c *config, oldManifest, newManifest *Manifest, numWorkers int) ([]Delta, error) {
	deltas, err := findDeltas(c, oldManifest, newManifest)
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to create deltas list %s", newManifest.Name)
	}

	if len(deltas) == 0 {
		return []Delta{}, nil
	}

	if numWorkers < 1 {
		numWorkers = 1
	}
	var deltaQueue = make(chan *Delta)
	var wg sync.WaitGroup
	wg.Add(numWorkers)

	// Delta creation takes a lot of memory, so create a limited amount of goroutines.
	for i := 0; i < numWorkers; i++ {
		go func() {
			defer wg.Done()
			for delta := range deltaQueue {
				delta.Error = createDelta(c, delta)
			}
		}()
	}

	// Send jobs to the queue for delta goroutines to pick up.
	for i := range deltas {
		deltaQueue <- &deltas[i]
	}

	// Send message that no more jobs are being sent
	close(deltaQueue)
	wg.Wait()

	return deltas, nil
}

func createDelta(c *config, delta *Delta) error {
	if _, err := os.Stat(delta.Path); err == nil {
		// Skip existing deltas. Not verifying since client is resilient about that.
		return nil
	}

	oldPath := filepath.Join(c.imageBase, fmt.Sprint(delta.from.Version), "full", delta.from.Name)
	newPath := filepath.Join(c.imageBase, fmt.Sprint(delta.to.Version), "full", delta.to.Name)

	if err := helpers.RunCommandSilent("bsdiff", oldPath, newPath, delta.Path); err != nil {
		_ = os.Remove(delta.Path)
		if exitErr, ok := errors.Cause(err).(*exec.ExitError); ok {
			// bsdiff returns 1 that stands for "FULLDL", i.e. it decided that
			// a delta is not worth. Give a better error message for that case.
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				if status == 1 {
					return fmt.Errorf("bsdiff returned FULLDL, not using delta")
				}
			}
		}
		return errors.Wrapf(err, "Failed to create delta for %s (%d) -> %s (%d)", delta.from.Name, delta.from.Version, delta.to.Name, delta.to.Version)
	}

	// Check that the delta actually applies correctly.
	testPath := delta.Path + ".testnewfile"
	if err := helpers.RunCommandSilent("bspatch", oldPath, testPath, delta.Path); err != nil {
		return errors.Wrapf(err, "Failed to apply delta %s", delta.Path)
	}
	defer func() {
		_ = os.Remove(testPath)
	}()

	testHash, err := Hashcalc(testPath)
	if err != nil {
		_ = os.Remove(delta.Path)
		return errors.Wrap(err, "Failed to calculate hash for test file created applying delta")
	}
	if testHash != delta.to.Hash {
		_ = os.Remove(delta.Path)
		return errors.Wrapf(err, "Delta mismatch: %s -> %s via delta: %s", oldPath, newPath, delta.Path)
	}

	return nil
}

func findDeltas(c *config, oldManifest, newManifest *Manifest) ([]Delta, error) {
	oldManifest.sortFilesName()
	newManifest.sortFilesName()

	err := linkDeltaPeersForPack(c, oldManifest, newManifest)
	if err != nil {
		return nil, err
	}

	deltaCount := 0
	for _, nf := range newManifest.Files {
		if nf.DeltaPeer != nil {
			deltaCount++
		}
	}

	deltas := make([]Delta, 0, deltaCount)

	// Use set to remove completely equal delta entries. These happen when two files that look
	// the same, change content in next version (but still look the same).
	seen := make(map[string]bool)

	for _, nf := range newManifest.Files {
		if nf.DeltaPeer == nil {
			continue
		}

		from := nf.DeltaPeer
		to := nf
		dir := filepath.Join(c.outputDir, fmt.Sprint(to.Version), "delta")
		name := fmt.Sprintf("%d-%d-%s-%s", from.Version, to.Version, from.Hash, to.Hash)
		path := filepath.Join(dir, name)

		if seen[path] {
			continue
		}

		seen[path] = true
		deltas = append(deltas, Delta{
			Path: path,
			from: from,
			to:   to,
		})
	}

	return deltas, nil
}
