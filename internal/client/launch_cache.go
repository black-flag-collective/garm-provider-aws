package client

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/cloudbase/garm-provider-aws/internal/spec"
)

// Capacity errors must reach our placement loop without SDK backoff. Other
// errors retain the configured retry policy, including throttling retries.
type placementRetryer struct{ aws.Retryer }

func (r placementRetryer) IsErrorRetryable(err error) bool {
	if isCapacityError(err) {
		return false
	}
	return r.Retryer.IsErrorRetryable(err)
}

type cacheEntry struct {
	Value   string    `json:"value"`
	Expires time.Time `json:"expires"`
}

// Each entry is replaced atomically. Concurrent provider processes can read
// without a shared lock; losing a duplicate write only causes a cache miss.
func (a *AwsCli) cachePath(key string) string {
	return filepath.Join(a.cacheDir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(a.cfg.Region+"/"+key))))
}
func (a *AwsCli) cached(key string) string {
	if a.cacheDir == "" {
		return ""
	}
	data, err := os.ReadFile(a.cachePath(key))
	if err != nil {
		return ""
	}
	var entry cacheEntry
	if json.Unmarshal(data, &entry) != nil || !time.Now().Before(entry.Expires) {
		return ""
	}
	return entry.Value
}
func (a *AwsCli) remember(key, value string, ttl time.Duration) {
	if a.cacheDir == "" {
		return
	}
	if os.MkdirAll(a.cacheDir, 0700) != nil {
		return
	}
	data, err := json.Marshal(cacheEntry{Value: value, Expires: time.Now().Add(ttl)})
	if err != nil {
		return
	}
	f, err := os.CreateTemp(a.cacheDir, ".entry-")
	if err != nil {
		return
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	closeErr := f.Close()
	if err == nil && closeErr == nil {
		_ = os.Rename(f.Name(), a.cachePath(key))
	}
}
func placementKey(s *spec.RunnerSpec, p spec.LaunchAttempt) string {
	return fmt.Sprintf("capacity/%s/%t/%s/%s", s.ControllerID, p.Spot, p.Flavor, p.SubnetID)
}
func (a *AwsCli) launchAttempts(s *spec.RunnerSpec) []spec.LaunchAttempt {
	attempts := s.LaunchAttempts()
	refused := make(map[spec.LaunchAttempt]bool)
	for _, p := range attempts {
		refused[p] = a.cached(placementKey(s, p)) != ""
	}
	// Keep spot before on-demand, and preserve configured order within each
	// market except that recently refused placements go last.
	sort.SliceStable(attempts, func(i, j int) bool {
		if attempts[i].Spot != attempts[j].Spot {
			return attempts[i].Spot
		}
		return !refused[attempts[i]] && refused[attempts[j]]
	})
	return attempts
}
