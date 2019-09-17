/*
 * Copyright The Dragonfly Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package dfgettask

import (
	"context"
	"fmt"
	"strings"

	"github.com/dragonflyoss/Dragonfly/apis/types"
	"github.com/dragonflyoss/Dragonfly/pkg/errortypes"
	"github.com/dragonflyoss/Dragonfly/pkg/metricsutils"
	"github.com/dragonflyoss/Dragonfly/pkg/stringutils"
	"github.com/dragonflyoss/Dragonfly/pkg/syncmap"
	"github.com/dragonflyoss/Dragonfly/supernode/config"
	"github.com/dragonflyoss/Dragonfly/supernode/daemon/mgr"
	dutil "github.com/dragonflyoss/Dragonfly/supernode/daemon/util"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

var _ mgr.DfgetTaskMgr = &Manager{}

const (
	keyJoinChar = "@"
)

type metrics struct {
	dfgetTasks              *prometheus.GaugeVec
	dfgetTasksRegisterCount *prometheus.CounterVec
	dfgetTasksFailCount     *prometheus.CounterVec
}

func newMetrics(register prometheus.Registerer) *metrics {
	return &metrics{
		dfgetTasks: metricsutils.NewGauge(config.SubsystemSupernode, "dfgettasks",
			"Current status of dfgettasks", []string{"callsystem", "status"}, register),

		dfgetTasksRegisterCount: metricsutils.NewCounter(config.SubsystemSupernode, "dfgettasks_registered_total",
			"Total times of registering dfgettasks", []string{"callsystem"}, register),

		dfgetTasksFailCount: metricsutils.NewCounter(config.SubsystemSupernode, "dfgettasks_failed_total",
			"Total failure times of dfgettasks", []string{"callsystem"}, register),
	}
}

// Manager is an implementation of the interface of DfgetTaskMgr.
type Manager struct {
	cfg            *config.Config
	dfgetTaskStore *dutil.Store
	ptoc           *syncmap.SyncMap
	metrics        *metrics
}

// NewManager returns a new Manager.
func NewManager(cfg *config.Config, register prometheus.Registerer) (*Manager, error) {
	return &Manager{
		cfg:            cfg,
		dfgetTaskStore: dutil.NewStore(),
		ptoc:           syncmap.NewSyncMap(),
		metrics:        newMetrics(register),
	}, nil
}

// Add a new dfgetTask, we use clientID and taskID to identify a dfgetTask uniquely.
// ClientID should be generated by dfget, supernode will use it directly.
// NOTE: We should create a new dfgetTask for each download process,
//       even if the downloads initiated by the same machine.
func (dtm *Manager) Add(ctx context.Context, dfgetTask *types.DfGetTask) error {
	if stringutils.IsEmptyStr(dfgetTask.Path) {
		return errors.Wrapf(errortypes.ErrEmptyValue, "Path")
	}

	if stringutils.IsEmptyStr(dfgetTask.PeerID) {
		return errors.Wrapf(errortypes.ErrEmptyValue, "PeerID")
	}

	key, err := generateKey(dfgetTask.CID, dfgetTask.TaskID)
	if err != nil {
		return err
	}

	// the default status of DfgetTask is WAITING
	if stringutils.IsEmptyStr(dfgetTask.Status) {
		dfgetTask.Status = types.DfGetTaskStatusWAITING
	}

	// TODO: should we verify that the peerID is valid here.

	dtm.ptoc.Add(generatePeerKey(dfgetTask.PeerID, dfgetTask.TaskID), dfgetTask.CID)
	dtm.dfgetTaskStore.Put(key, dfgetTask)

	// If dfget task is created by supernode cdn, don't update metrics.
	if !dtm.cfg.IsSuperPID(dfgetTask.PeerID) || !dtm.cfg.IsSuperCID(dfgetTask.CID) {
		dtm.metrics.dfgetTasks.WithLabelValues(dfgetTask.CallSystem, dfgetTask.Status).Inc()
		dtm.metrics.dfgetTasksRegisterCount.WithLabelValues(dfgetTask.CallSystem).Inc()
	}

	return nil
}

// Get a dfgetTask info with specified clientID and taskID.
func (dtm *Manager) Get(ctx context.Context, clientID, taskID string) (dfgetTask *types.DfGetTask, err error) {
	return dtm.getDfgetTask(clientID, taskID)
}

// GetCIDByPeerIDAndTaskID returns cid with specified peerID and taskID.
func (dtm *Manager) GetCIDByPeerIDAndTaskID(ctx context.Context, peerID, taskID string) (string, error) {
	return dtm.ptoc.GetAsString(generatePeerKey(peerID, taskID))
}

// GetCIDsByTaskID returns cids as a string slice with specified taskID.
func (dtm *Manager) GetCIDsByTaskID(ctx context.Context, taskID string) ([]string, error) {
	var result []string
	suffixString := keyJoinChar + taskID
	rangeFunc := func(k, v interface{}) bool {
		key, ok := k.(string)
		if !ok {
			return true
		}

		if !strings.HasSuffix(key, suffixString) {
			return true
		}
		cid, err := dtm.ptoc.GetAsString(key)
		if err != nil {
			logrus.Warnf("failed to get cid from ptoc with key(%s): %v", key, err)
			return true
		}

		result = append(result, cid)
		return true
	}
	dtm.ptoc.Range(rangeFunc)

	return result, nil
}

// GetCIDAndTaskIDsByPeerID returns a cid<->taskID map by specified peerID.
func (dtm *Manager) GetCIDAndTaskIDsByPeerID(ctx context.Context, peerID string) (map[string]string, error) {
	var result = make(map[string]string)
	prefixStr := peerID + keyJoinChar
	rangeFunc := func(k, v interface{}) bool {
		key, ok := k.(string)
		if !ok {
			return true
		}

		if !strings.HasPrefix(key, prefixStr) {
			return true
		}
		cid, err := dtm.ptoc.GetAsString(key)
		if err != nil {
			logrus.Warnf("failed to get cid from ptoc with key(%s): %v", key, err)
			return true
		}

		// get TaskID from the key
		splitResult := strings.Split(key, keyJoinChar)
		result[cid] = splitResult[len(splitResult)-1]
		return true
	}
	dtm.ptoc.Range(rangeFunc)

	return result, nil
}

// List returns the list of dfgetTask.
func (dtm *Manager) List(ctx context.Context, filter map[string]string) (dfgetTaskList []*types.DfGetTask, err error) {
	return nil, nil
}

// Delete deletes a dfgetTask with clientID and taskID.
func (dtm *Manager) Delete(ctx context.Context, clientID, taskID string) error {
	key, err := generateKey(clientID, taskID)
	if err != nil {
		return err
	}

	dfgetTask, err := dtm.getDfgetTask(clientID, taskID)
	if err != nil {
		return err
	}
	dtm.ptoc.Delete(generatePeerKey(dfgetTask.PeerID, dfgetTask.TaskID))
	if !dtm.cfg.IsSuperCID(clientID) {
		dtm.metrics.dfgetTasks.WithLabelValues(dfgetTask.CallSystem, dfgetTask.Status).Dec()
	}
	return dtm.dfgetTaskStore.Delete(key)
}

// UpdateStatus updates the status of dfgetTask with specified clientID and taskID.
func (dtm *Manager) UpdateStatus(ctx context.Context, clientID, taskID, status string) error {
	dfgetTask, err := dtm.getDfgetTask(clientID, taskID)
	if err != nil {
		return err
	}

	if dfgetTask.Status != types.DfGetTaskStatusSUCCESS {
		dtm.metrics.dfgetTasks.WithLabelValues(dfgetTask.CallSystem, dfgetTask.Status).Dec()
		dtm.metrics.dfgetTasks.WithLabelValues(dfgetTask.CallSystem, status).Inc()
		dfgetTask.Status = status
	}

	// Add the total failed count.
	if dfgetTask.Status == types.DfGetTaskStatusFAILED {
		dtm.metrics.dfgetTasksFailCount.WithLabelValues(dfgetTask.CallSystem).Inc()
	}

	return nil
}

// getDfgetTask gets a DfGetTask from dfgetTaskStore with specified clientID and taskID.
func (dtm *Manager) getDfgetTask(clientID, taskID string) (*types.DfGetTask, error) {
	key, err := generateKey(clientID, taskID)
	if err != nil {
		return nil, err
	}

	v, err := dtm.dfgetTaskStore.Get(key)
	if err != nil {
		return nil, err
	}

	if dfgetTask, ok := v.(*types.DfGetTask); ok {
		return dfgetTask, nil
	}
	return nil, errors.Wrapf(errortypes.ErrConvertFailed, "clientID: %s, taskID: %s: %v", clientID, taskID, v)
}

// generateKey generates a key for a dfgetTask.
func generateKey(cID, taskID string) (string, error) {
	if stringutils.IsEmptyStr(cID) {
		return "", errors.Wrapf(errortypes.ErrEmptyValue, "cID")
	}

	if stringutils.IsEmptyStr(taskID) {
		return "", errors.Wrapf(errortypes.ErrEmptyValue, "taskID")
	}

	return fmt.Sprintf("%s%s%s", cID, keyJoinChar, taskID), nil
}

func generatePeerKey(peerID, taskID string) string {
	return fmt.Sprintf("%s%s%s", peerID, keyJoinChar, taskID)
}
