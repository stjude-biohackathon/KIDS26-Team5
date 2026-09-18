package types

import (
	"encoding/json"
	"fmt"
	"time"
)

type JobAddDto struct {
	UserId uint `json:"user_id"`

	PipelineName    string          `json:"pipeline_name" binding:"required,nonblank"`
	PipelineVersion string          `json:"pipeline_version" binding:"required,nonblank"`
	PipelineParams  json.RawMessage `json:"pipeline_params" binding:"required,nonblank"`

	// StorageConfigId names which storage the run should use. Zero means "my
	// default", which resolves to group storage ahead of personal.
	StorageConfigId uint `json:"storage_config_id,omitempty"`
}

func (j *JobAddDto) JobId() string {
	return fmt.Sprintf("%s-%s", j.PipelineName, j.PipelineVersion)
}

type JobDeleteDto struct {
	UserId uint `json:"user_id"`
	JobId  uint `json:"job_id" binding:"required,nonblank"`
}

type JobStopDto struct {
	UserId uint `json:"user_id"`
	JobId  uint `json:"job_id" binding:"required,nonblank"`
}

type JobListDto struct {
	ID              uint      `json:"id"`
	PipelineName    string    `json:"pipeline_name"`
	PipelineVersion string    `json:"pipeline_version"`
	CreatedAt       time.Time `json:"created_at"`
	Status          string    `json:"status"`
	DispatchID      string    `json:"dispatch_id"`
	AllocID         string    `json:"alloc_id"`
}

type JobDetailDto struct {
	ID              uint            `json:"id"`
	PipelineName    string          `json:"pipeline_name"`
	PipelineVersion string          `json:"pipeline_version"`
	CreatedAt       time.Time       `json:"created_at"`
	Status          string          `json:"status"`
	DispatchID      string          `json:"dispatch_id"`
	AllocID         string          `json:"alloc_id"`
	Params          json.RawMessage `json:"params"`
}
