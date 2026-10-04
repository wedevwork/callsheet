package contract

import (
	"bytes"
	"encoding/json"
)

// The sidecar's publication checkpoint (iteration 10b):
// tasks/<task_id>/publication.json, schema 1, written by the task's worker
// with the journal's atomic temporary/sync/rename/directory-sync discipline
// as each step of a workspace task's publication becomes durable: the
// sealed outcome candidate and result tree (sealed), the plane's authorized
// intent and the deterministic commit hash (authorized), then the plane's
// canonical settlement (settled). A restarted sidecar resumes from it: a
// sealed candidate may still Begin (the plane decides), an authorized
// intent is only observed (never pushed again), a settled one is the
// outbox's result. It carries no contents.
const (
	PublicationCheckpointSchema = 1
	MaxPublicationCheckpoint    = 1 << 20

	CheckpointSealed     = "sealed"
	CheckpointAuthorized = "authorized"
	CheckpointSettled    = "settled"
)

// PublicationCheckpoint is publication.json.
type PublicationCheckpoint struct {
	TaskID        string         `json:"task_id"`
	Execution     ExecutionToken `json:"execution"`
	Phase         string         `json:"phase"`
	Candidate     TaskResultBody `json:"candidate"`
	Tree          string         `json:"tree"`
	PublicationID *string        `json:"publication_id"`
	State         *string        `json:"state"`
	ExpiresAt     *string        `json:"expires_at"`
	Commit        *string        `json:"commit"`
	// Pushed records that a push began (the receive outcome is observed,
	// never retried).
	Pushed bool            `json:"pushed"`
	Result *TaskResultBody `json:"result"`
}

type checkpointWire struct {
	SchemaVersion int `json:"schema_version"`
	PublicationCheckpoint
}

// EncodePublicationCheckpoint renders a validated checkpoint (two-space
// indented, final LF).
func EncodePublicationCheckpoint(c PublicationCheckpoint) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(checkpointWire{PublicationCheckpointSchema, c}); err != nil {
		return nil, err
	}
	if b.Len() > MaxPublicationCheckpoint {
		return nil, errInvalid("publication checkpoint exceeds %d bytes", MaxPublicationCheckpoint)
	}
	return b.Bytes(), nil
}

// ParsePublicationCheckpoint strictly decodes and validates publication.json.
func ParsePublicationCheckpoint(data []byte) (PublicationCheckpoint, error) {
	const what = "publication checkpoint"
	if len(data) > MaxPublicationCheckpoint {
		return PublicationCheckpoint{}, errInvalid("%s exceeds %d bytes", what, MaxPublicationCheckpoint)
	}
	var w struct {
		SchemaVersion int             `json:"schema_version"`
		TaskID        string          `json:"task_id"`
		Execution     ExecutionToken  `json:"execution"`
		Phase         string          `json:"phase"`
		Candidate     TaskResultBody  `json:"candidate"`
		Tree          string          `json:"tree"`
		PublicationID *string         `json:"publication_id"`
		State         *string         `json:"state"`
		ExpiresAt     *string         `json:"expires_at"`
		Commit        *string         `json:"commit"`
		Pushed        bool            `json:"pushed"`
		Result        *TaskResultBody `json:"result"`
	}
	if err := decodeStrict(data, &w, what); err != nil {
		return PublicationCheckpoint{}, err
	}
	if w.SchemaVersion != PublicationCheckpointSchema {
		return PublicationCheckpoint{}, errInvalid("%s schema_version %d is not supported (this build supports %d)", what, w.SchemaVersion, PublicationCheckpointSchema)
	}
	c := PublicationCheckpoint{TaskID: w.TaskID, Execution: w.Execution, Phase: w.Phase, Candidate: w.Candidate, Tree: w.Tree, PublicationID: w.PublicationID,
		State: w.State, ExpiresAt: w.ExpiresAt, Commit: w.Commit, Pushed: w.Pushed, Result: w.Result}
	return c, c.Validate()
}

// Validate checks identity, the candidate and the phase invariants.
func (c PublicationCheckpoint) Validate() error {
	const what = "publication checkpoint"
	switch {
	case !ValidTaskID(c.TaskID):
		return errInvalid("%s task_id is not a task ID", what)
	case c.Candidate.TaskID != c.TaskID || c.Candidate.Execution != c.Execution || c.Candidate.Workspace != nil:
		return errInvalid("%s candidate names another execution or carries a workspace result", what)
	case !ValidCommitHash(c.Tree):
		return errInvalid("%s tree must be a full hash", what)
	}
	if err := c.Candidate.Validate(); err != nil {
		return err
	}
	intent := c.PublicationID != nil && ValidWorkspaceToken(*c.PublicationID) && c.State != nil && publishableState(*c.State) &&
		c.ExpiresAt != nil && validTime(*c.ExpiresAt) && c.Commit != nil && ValidCommitHash(*c.Commit)
	none := c.PublicationID == nil && c.State == nil && c.ExpiresAt == nil && c.Commit == nil
	switch c.Phase {
	case CheckpointSealed:
		if !none || c.Pushed || c.Result != nil {
			return errInvalid("%s: a sealed checkpoint has no intent, push or result", what)
		}
	case CheckpointAuthorized:
		if !intent || c.Result != nil {
			return errInvalid("%s: an authorized checkpoint has its complete intent and no result", what)
		}
	case CheckpointSettled:
		if c.Result == nil || c.Result.Workspace == nil || c.Result.TaskID != c.TaskID || c.Result.Execution != c.Execution || (!intent && !none) {
			return errInvalid("%s: a settled checkpoint holds this execution's canonical result", what)
		}
		if err := c.Result.Validate(); err != nil {
			return err
		}
	default:
		return errInvalid("%s phase must be sealed, authorized or settled", what)
	}
	return nil
}
