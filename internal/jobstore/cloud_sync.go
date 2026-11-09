package jobstore

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"
)

const CloudSyncFile = "cloud_sync.json"

const (
	CloudPending     = "pending"
	CloudUnavailable = "unavailable"
	CloudConfirmed   = "confirmed"
	CloudFailed      = "failed"
)

// CloudIntent is published with the job before the supervisor can run AGY.
type CloudIntent struct {
	ProjectID string        `json:"project_id"`
	Title     string        `json:"title,omitempty"`
	Baseline  CloudRevision `json:"baseline"`
}

type CloudRevision struct {
	Modified time.Time `json:"modified"`
	Steps    int       `json:"steps"`
}

// CloudSync is the worker's receipt for the job's conversation revision.
type CloudSync struct {
	State          string        `json:"state"`
	Reason         string        `json:"reason,omitempty"`
	ConversationID string        `json:"conversation_id,omitempty"`
	Title          string        `json:"title,omitempty"`
	Revision       CloudRevision `json:"revision"`
	Attempts       int           `json:"attempts,omitempty"`
	RetryAt        time.Time     `json:"retry_at,omitzero"`
	SettledAt      time.Time     `json:"settled_at,omitzero"`
}

func (s CloudSync) Settled() bool {
	return s.State == CloudConfirmed || s.State == CloudFailed
}

func ReadCloudSync(dir string) (CloudSync, error) {
	var receipt CloudSync
	b, _, err := readReplacedFile(filepath.Join(dir, CloudSyncFile), 64<<10)
	if err != nil {
		return receipt, err
	}
	if err := json.Unmarshal(b, &receipt); err != nil {
		return CloudSync{}, err
	}
	switch receipt.State {
	case CloudPending, CloudUnavailable, CloudFailed, CloudConfirmed:
	default:
		return CloudSync{}, fmt.Errorf("unknown cloud sync state %q", receipt.State)
	}
	if receipt.Settled() && receipt.SettledAt.IsZero() {
		return CloudSync{}, fmt.Errorf("settled cloud sync receipt has no completion time")
	}
	if receipt.State == CloudConfirmed && (receipt.ConversationID == "" || receipt.Title == "" || receipt.Revision.Modified.IsZero()) {
		return CloudSync{}, fmt.Errorf("confirmed cloud sync receipt has incomplete revision evidence")
	}
	return receipt, nil
}

func WriteCloudSync(dir string, receipt CloudSync) error {
	b, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	return WritePrivateFileAtomic(dir, CloudSyncFile, b)
}
