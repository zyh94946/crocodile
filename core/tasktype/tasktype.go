package tasktype

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	pb "github.com/labulaka521/crocodile/core/proto"
	"github.com/labulaka521/crocodile/core/utils/define"
)

const (
	// DefaultExitCode default err code if not get run task code
	DefaultExitCode int = -1
)

// TaskRuner run task interface
// Please Implment io.ReadCloser
// reader last 3 byte must be exit code
type TaskRuner interface {
	Run(ctx context.Context) (out io.ReadCloser)
}

// GetDataRun get task type
// get api or shell
func GetDataRun(t *pb.TaskReq) (TaskRuner, error) {

	switch define.TaskType(t.TaskType) {
	case define.Shell:
		var shell DataShell
		err := json.Unmarshal(t.TaskData, &shell)
		if err != nil {
			return nil, err
		}
		return &shell, err

	case define.API:
		var api DataAPI
		err := json.Unmarshal(t.TaskData, &api)
		if err != nil {
			return nil, err
		}
		return &api, err

	default:
		err := fmt.Errorf("Unsupport TaskType %d", t.TaskType)
		return nil, err
	}
}