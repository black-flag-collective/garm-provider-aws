package client

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func claimInstanceIDs(claim runnerClaim) []string {
	ids := []string{}
	if claim.ID != "" && claim.Starting {
		ids = append(ids, claim.ID)
	}
	if claim.Cold != nil && claim.Cold.ID != "" {
		ids = append(ids, claim.Cold.ID)
	}
	return ids
}

// Keep an O(1) reverse lookup for deletion; scanning all historic runners on
// every job completion would grow with the age of the controller.
func indexRunnerClaim(lock *os.File, id string) error {
	if filepath.Base(id) != id {
		return fmt.Errorf("invalid claim index ID")
	}
	dir := filepath.Join(filepath.Dir(filepath.Dir(lock.Name())), "claim-index")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, id)
	name := filepath.Base(lock.Name())
	if data, err := os.ReadFile(path); err == nil {
		if string(data) != name {
			return fmt.Errorf("instance already indexed to another runner")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	temp, err := os.CreateTemp(dir, ".index-")
	if err != nil {
		return err
	}
	defer temp.Close()
	defer os.Remove(temp.Name())
	if _, err := temp.WriteString(name); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Link(temp.Name(), path); err != nil {
		return err
	}
	return syncDirectory(dir)
}

// EC2 termination success (or NotFound) is definitive. Rotate the runner's
// generation under its existing lock, retaining instance tombstones and index
// entries so a late repeated delete cannot clear a newer runner's record.
func (a *AwsCli) retireRunnerClaim(id string) error {
	if filepath.Base(id) != id {
		return fmt.Errorf("invalid retired instance ID")
	}
	if a.cacheDir == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(a.cacheDir, "claim-index", id))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	name := string(data)
	if filepath.Base(name) != name || !strings.HasSuffix(name, ".lock") {
		return fmt.Errorf("invalid runner claim index")
	}
	lock, err := os.OpenFile(filepath.Join(a.cacheDir, "runner-claims", name), os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lockClaim(lock); err != nil {
		return err
	}
	data, err = os.ReadFile(lock.Name() + ".json")
	if err != nil {
		return err
	}
	var claim runnerClaim
	if err := json.Unmarshal(data, &claim); err != nil {
		return err
	}
	activeID := claim.ID
	if claim.Cold != nil {
		activeID = claim.Cold.ID
	}
	if activeID != id {
		return nil
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	return saveRunnerClaim(lock, runnerClaim{Generation: hex.EncodeToString(nonce[:])})
}
