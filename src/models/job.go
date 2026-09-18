package models

import (
	"encoding/json"

	"gorm.io/gorm"
)

type Job struct {
	gorm.Model

	DispatchId string          `gorm:"index;comment:'nomad dispatched job id'" json:"dispatch_id"`
	AllocId    string          `gorm:"type:varchar(64);index;comment:'nomad task allocation id'" json:"alloc_id"`
	UserId     *uint           `gorm:"index;comment:'user id'" json:"user_id"`
	PipelineId *uint           `gorm:"index;comment:'pipeline id'" json:"pipeline_id"`

	// GroupId attributes the run's compute cost to a lab or grant. Reporting
	// only — job authorization is still ownership-based via CheckJobOwnership.
	//
	// Stamped at submit time because it cannot be reconstructed later: once a
	// storage config is re-granted or edited, there is no way to work backwards
	// to the group a past run belonged to.
	GroupId *uint `gorm:"index;comment:'group the run is attributed to'" json:"group_id,omitempty"`

	// StorageConfigId records which storage the run used, so a later audit can
	// tell where its inputs and outputs actually live.
	StorageConfigId uint `gorm:"index;comment:'storage config used for this run'" json:"storage_config_id,omitempty"`
	Status     string          `gorm:"type:varchar(20);not null"` // values: "submitted", "dispatch_success", "pending", "running", "completed", "failed"
	Params     json.RawMessage `gorm:"type:jsonb;comment:'pipeline input parameters'" json:"params"`

	// auxiliary field used for address the deletion of user/pipeline
	UserEmail       string `gorm:"type:varchar(30)" json:"user_email"`
	PipelineName    string `gorm:"type:varchar(64)" json:"pipeline_name"`
	PipelineVersion string `gorm:"type:varchar(20)" json:"pipeline_version"`

	User     *User     `gorm:"foreignKey:UserId" json:"user,omitempty"`
	Pipeline *Pipeline `gorm:"foreignKey:PipelineId;" json:"pipeline,omitempty"`
}

// JobTemplate stores Nomad HCL templates for different job engines.
// Templates are stored in the database so they can be managed via the admin UI
// without redeployment.
type JobTemplate struct {
	gorm.Model
	Name        string `gorm:"type:varchar(64);uniqueIndex;not null" json:"name"`
	Engine      string `gorm:"type:varchar(32);not null" json:"engine"` // "nextflow", "wdl", "script"
	Version     string `gorm:"type:varchar(32)" json:"version"`
	Description string `gorm:"type:text" json:"description"`
	HCLTemplate string `gorm:"type:text;not null" json:"hcl_template"`
	Schema      string `gorm:"type:text" json:"schema,omitempty"`   // JSON Schema for params validation
	Defaults    string `gorm:"type:text" json:"defaults,omitempty"` // Default meta/env as JSON
	IsBuiltIn   bool   `gorm:"default:false" json:"is_built_in"`
}

func (j *Job) BeforeCreate(tx *gorm.DB) (err error) {
	if j.UserId != nil {
		var user User
		if err = tx.First(&user, j.UserId).Error; err == nil {
			j.UserEmail = user.Email
		}
	}

	if j.PipelineId != nil {
		var pipeline Pipeline
		if err = tx.First(&pipeline, j.PipelineId).Error; err == nil {
			j.PipelineName = pipeline.Name
			j.PipelineVersion = pipeline.Version
		}
	}

	return nil
}
