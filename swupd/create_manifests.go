// Copyright 2017 Intel Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package swupd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// UpdateInfo contains the meta information for the current update
type UpdateInfo struct {
	oldFormat   uint
	format      uint
	lastVersion uint32
	minVersion  uint32
	version     uint32
	bundles     []string
	timeStamp   time.Time
}

func initBuildEnv(c config) error {
	tmpDir := filepath.Join(c.stateDir, "temp")
	// remove old directory
	if err := os.RemoveAll(tmpDir); err != nil {
		return err
	}

	// create new one
	return os.Mkdir(tmpDir, os.ModePerm)
}

func getOldManifest(path string) (*Manifest, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return &Manifest{}, nil
	}
	return ParseManifestFile(path)
}

func processBundles(ui UpdateInfo, c config) ([]*Manifest, error) {
	var newFull *Manifest
	var err error
	tmpManifests := []*Manifest{}
	totalBundles := len(ui.bundles)
	// first loop sets up initial bundle manifests and adds files to them
	for i, bundleName := range ui.bundles {
		fmt.Printf("[%d/%d] %s\n", i+1, totalBundles, bundleName)
		bundle := &Manifest{
			Header: ManifestHeader{
				Format:    ui.format,
				Version:   ui.version,
				Previous:  ui.lastVersion,
				TimeStamp: ui.timeStamp,
			},
			Name: bundleName,
		}

		if bundleName == "full" {
			// track full manifest so we can do extra processing later
			// (maximizeFull)
			newFull = bundle
			chroot := filepath.Join(c.imageBase, fmt.Sprint(ui.version), "full")
			err = newFull.addFilesFromChroot(chroot)
		} else {
			biPath := filepath.Join(c.imageBase, fmt.Sprint(ui.version), bundle.Name+"-info")
			useBundleInfo := true
			if _, err = os.Stat(biPath); os.IsNotExist(err) {
				err = syncToFull(ui.version, bundle.Name, c.imageBase)
				if err != nil {
					return nil, err
				}
				useBundleInfo = false
			}

			err = bundle.getBundleInfo(biPath)
			if err != nil {
				return nil, err
			}

			if useBundleInfo {
				err = bundle.addFilesFromBundleInfo(c, ui.version)
			}
		}
		if err != nil {
			return nil, err
		}

		// detect type changes
		// fail out here if a type change is detected since this is not yet supported in client
		if bundle.hasUnsupportedTypeChanges() {
			return nil, errors.New("type changes not yet supported")
		}

		// remove banned debuginfo if configured to do so
		if c.debuginfo.banned {
			bundle.removeDebuginfo(c.debuginfo)
		}

		bundle.sortFilesName()
		tmpManifests = append(tmpManifests, bundle)
	}

	// second loop reads includes
	for _, bundle := range tmpManifests {
		if bundle.Name != "os-core" && bundle != newFull {
			// read in bundle includes
			if err = bundle.readIncludesFromBundleInfo(tmpManifests); err != nil {
				return nil, err
			}
		}
	}

	// final loop performs manifest subtraction. Important this is done after all includes
	// have been read so nested subtraction works.
	for _, bundle := range tmpManifests {
		bundle.subtractManifests(bundle)
	}

	// Need old MoM to get version of last bundle manifest
	oldMoMPath := filepath.Join(c.outputDir, fmt.Sprint(ui.lastVersion), "Manifest.MoM")
	oldMoM, err := getOldManifest(oldMoMPath)
	if err != nil {
		return nil, err
	}

	// final loop detects changes, applies heuristics to files, and sorts the file lists
	newManifests := []*Manifest{}
	for _, bundle := range tmpManifests {
		// Check for changed includes, changed or added or deleted files
		// must be done after subtractManifests because the oldM is a subtracted
		// manifest
		ver := getManifestVerFromMoM(oldMoM, bundle)
		if ver == 0 {
			ver = ui.lastVersion
		}

		oldMPath := filepath.Join(c.outputDir, fmt.Sprint(ver), "Manifest."+bundle.Name)
		oldM, err := getOldManifest(oldMPath)
		if err != nil {
			return nil, err
		}
		changedIncludes := compareIncludes(bundle, oldM)
		oldM.sortFilesName()
		changedFiles, added, deleted := bundle.linkPeersAndChange(oldM, c, ui.minVersion)
		// if nothing changed, skip
		if changedFiles == 0 && added == 0 && deleted == 0 && !changedIncludes {
			continue
		}

		// detect modifier flag for all files in the manifest
		// must happen after finding newDeleted files to catch ghosted files.
		bundle.applyHeuristics()
		// Assign final FileCount based on the files that made it this far
		bundle.Header.FileCount = uint32(len(bundle.Files))
		// If we made it this far, this bundle has a change and should be written
		newManifests = append(newManifests, bundle)
	}

	// maximize full manifest while all the manifests are still sorted by name
	maximizeFull(newFull, newManifests)

	return newManifests, nil
}

// CreateManifests creates update manifests for changed and added bundles for <version>
func CreateManifests(version uint32, minVersion uint32, format uint, statedir string) (*MoM, error) {
	var err error
	var c config

	if minVersion > version {
		return nil, fmt.Errorf("minVersion (%v), must be between 0 and %v (inclusive)",
			minVersion, version)
	}

	c, err = getConfig(statedir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Found server.ini, but was unable to read it. "+
			"Continuing with default configuration\n")
	}

	if err = initBuildEnv(c); err != nil {
		return nil, err
	}

	var groups []string
	if groups, err = readGroupsINI(filepath.Join(c.stateDir, "groups.ini")); err != nil {
		return nil, err
	}

	groups = append(groups, "full")

	var lastVersion uint32
	lastVersion, err = readLastVerFile(filepath.Join(c.imageBase, "LAST_VER"))
	if err != nil {
		return nil, err
	}

	oldFullManifestPath := filepath.Join(c.outputDir, fmt.Sprint(lastVersion), "Manifest.full")
	oldFullManifest, err := ParseManifestFile(oldFullManifestPath)
	if err != nil {
		// throw away read manifest if it is invalid
		if strings.Contains(err.Error(), "invalid manifest") {
			fmt.Fprintf(os.Stderr, "full: %s\n", err)
		}
		oldFullManifest = &Manifest{}
	}

	if oldFullManifest.Header.Format > format {
		return nil, fmt.Errorf("new format %v is lower than old format %v", format, oldFullManifest.Header.Format)
	}

	timeStamp := time.Now()
	oldMoMPath := filepath.Join(c.outputDir, fmt.Sprint(lastVersion), "Manifest.MoM")
	oldMoM, err := getOldManifest(oldMoMPath)
	if err != nil {
		return nil, err
	}
	oldFormat := oldMoM.Header.Format

	// PROCESS BUNDLES
	ui := UpdateInfo{
		oldFormat:   oldFormat,
		format:      format,
		lastVersion: lastVersion,
		minVersion:  minVersion,
		version:     version,
		bundles:     groups,
		timeStamp:   timeStamp,
	}
	var newManifests []*Manifest
	if newManifests, err = processBundles(ui, c); err != nil {
		return nil, err
	}

	verOutput := filepath.Join(c.outputDir, fmt.Sprint(version))
	if err = os.MkdirAll(verOutput, 0755); err != nil {
		return nil, err
	}

	// Bootstrap delta directory, so we can assume every version will have one.
	if err = os.MkdirAll(filepath.Join(verOutput, "delta"), 0755); err != nil {
		return nil, err
	}

	newMoM := Manifest{
		Name: "MoM",
		Header: ManifestHeader{
			Format:    format,
			Version:   version,
			Previous:  lastVersion,
			TimeStamp: timeStamp,
		},
	}

	var newFull *Manifest
	// write manifests then add them to the MoM
	for _, bMan := range newManifests {
		// handle the full manifest last
		if bMan.Name == "full" {
			newFull = bMan
			continue
		}

		// TODO: remove this after a format bump in Clear Linux
		// this is a hack to set maximum contentsize to the incorrect maximum
		// set in swupd-client v3.15.3
		bMan.setMaxContentSizeHack()
		// end hack

		// sort by version then by filename, previously to this sort these bundles
		// were sorted by file name only to make processing easier
		bMan.sortFilesVersionName()
		manPath := filepath.Join(verOutput, "Manifest."+bMan.Name)
		if err = bMan.WriteManifestFile(manPath); err != nil {
			return nil, err
		}

		// add bundle to Manifest.MoM
		if err = newMoM.createManifestRecord(verOutput, manPath, version); err != nil {
			return nil, err
		}
	}

	// copy over unchanged manifests
	for _, m := range oldMoM.Files {
		if m.findFileNameInSlice(newMoM.Files) == nil {
			if m.Name == indexBundle {
				// this is generated new each time
				continue
			}
			newMoM.Files = append(newMoM.Files, m)
		}
	}

	// allManifests must include newManifests plus all old ones in the MoM.
	allManifests := newManifests
	// now append all old manifests
	for _, m := range newMoM.Files {
		if m.Version < version {
			oldMPath := filepath.Join(c.outputDir, fmt.Sprint(m.Version), "Manifest."+m.Name)
			var oldM *Manifest
			oldM, err = ParseManifestFile(oldMPath)
			if err != nil {
				return nil, err
			}
			allManifests = append(allManifests, oldM)
		}
	}

	var osIdx *Manifest
	if osIdx, err = writeIndexManifest(&c, &ui, allManifests); err != nil {
		return nil, err
	}

	osIdxPath := filepath.Join(verOutput, "Manifest."+osIdx.Name)
	if err = newMoM.createManifestRecord(verOutput, osIdxPath, version); err != nil {
		return nil, err
	}

	// track here as well so the manifest tar is made
	newManifests = append(newManifests, osIdx)

	// handle full manifest
	newFull.sortFilesVersionName()
	if err = newFull.WriteManifestFile(filepath.Join(verOutput, "Manifest.full")); err != nil {
		return nil, err
	}

	newMoM.Header.FileCount = uint32(len(newMoM.Files))
	newMoM.sortFilesVersionName()

	// write MoM
	if err = newMoM.WriteManifestFile(filepath.Join(verOutput, "Manifest.MoM")); err != nil {
		return nil, err
	}

	// Make the result MoM struct to return.
	result := &MoM{
		Manifest:       newMoM,
		UpdatedBundles: make([]*Manifest, 0, len(newManifests)),
	}
	for _, b := range newManifests {
		if b.Name == "full" {
			result.FullManifest = b
		} else {
			result.UpdatedBundles = append(result.UpdatedBundles, b)
		}
	}
	return result, nil
}
