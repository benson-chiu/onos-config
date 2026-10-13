// SPDX-FileCopyrightText: 2020-present Open Networking Foundation <info@opennetworking.org>
//
// SPDX-License-Identifier: Apache-2.0

package configuration

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/openconfig/gnmi/proto/gnmi_ext"

	controllerutils "github.com/onosproject/onos-config/pkg/controller/utils"

	topoapi "github.com/onosproject/onos-api/go/onos/topo"

	"github.com/onosproject/onos-lib-go/pkg/errors"

	configapi "github.com/onosproject/onos-api/go/onos/config/v2"
	utilsv2 "github.com/onosproject/onos-config/pkg/utils/v2/values"

	"github.com/onosproject/onos-config/pkg/southbound/gnmi"
	"github.com/onosproject/onos-config/pkg/store/topo"
	configuration "github.com/onosproject/onos-config/pkg/store/v2/configuration"
	proposalstore "github.com/onosproject/onos-config/pkg/store/v2/proposal"
	"github.com/onosproject/onos-lib-go/pkg/controller"
	"github.com/onosproject/onos-lib-go/pkg/logging"
)

var log = logging.GetLogger("controller", "configuration")

const (
	defaultTimeout = 30 * time.Second

	// maxSyncAttempts bounds how many times one transaction index is retried
	// before its values are rolled back. Each attempt can take as long as the
	// southbound timeout, so this is minutes of retrying rather than seconds.
	//
	// Without a bound a change the device will never accept keeps the
	// configuration in SYNCHRONIZING forever, and reconcileApply in the
	// proposal controller refuses to apply while that is the case: one bad
	// change blocks every later change to the same target.
	maxSyncAttempts = 6

	// syncRollbackPrefix marks a failure description as a rolled back change,
	// distinguishing it from one that is still being retried.
	syncRollbackPrefix = "rolled back: "
)

// syncAttempt counts consecutive synchronisation failures for one index.
type syncAttempt struct {
	index configapi.Index
	count int
}

// NewController returns a configuration controller
func NewController(topo topo.Store, conns gnmi.ConnManager, configurations configuration.Store, proposals proposalstore.Store) *controller.Controller {
	c := controller.NewController("configuration")
	c.Watch(&Watcher{
		configurations: configurations,
	})
	c.Watch(&TopoWatcher{
		topo: topo,
	})
	c.Reconcile(&Reconciler{
		conns:          conns,
		topo:           topo,
		configurations: configurations,
		proposals:      proposals,
		attempts:       make(map[configapi.ConfigurationID]*syncAttempt),
	})
	return c
}

// Reconciler reconciles configurations
type Reconciler struct {
	conns          gnmi.ConnManager
	topo           topo.Store
	configurations configuration.Store
	proposals      proposalstore.Store

	// attempts tracks synchronisation failures per configuration. It is
	// in-memory on purpose: a restart clears the counters, and a restart is
	// also the point at which the device state is read afresh.
	mu       sync.Mutex
	attempts map[configapi.ConfigurationID]*syncAttempt
}

// Reconcile reconciles target configurations
func (r *Reconciler) Reconcile(id controller.ID) (controller.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	configurationID := id.Value.(configapi.ConfigurationID)
	config, err := r.configurations.Get(ctx, configurationID)
	if err != nil {
		if !errors.IsNotFound(err) {
			log.Warnf("Failed to reconcile Configuration '%s'", configurationID, err)
			return controller.Result{}, err
		}
		log.Debugf("Configuration '%s' not found", configurationID)
		return controller.Result{}, nil
	}

	log.Debugf("Reconciling Configuration '%s'", config.ID)
	log.Debug(config)
	return r.reconcileConfiguration(ctx, config)
}

func (r *Reconciler) reconcileConfiguration(ctx context.Context, config *configapi.Configuration) (controller.Result, error) {
	// Get the target entity from topo
	target, err := r.topo.Get(ctx, topoapi.ID(config.TargetID))
	if err != nil {
		if !errors.IsNotFound(err) {
			log.Errorf("Failed fetching target Entity '%s' from topo", config.TargetID, err)
			return controller.Result{}, err
		}
		log.Debugf("Target entity '%s' not found", config.TargetID)
		return controller.Result{}, nil
	}

	// Get the target configurable configuration
	configurable := topoapi.Configurable{}
	_ = target.GetAspect(&configurable)

	// If the target is persistent, mark the configuration PERSISTED.
	if configurable.Persistent {
		if config.Status.State != configapi.ConfigurationStatus_PERSISTED {
			log.Infof("Skipping synchronization of Configuration '%s': target is persistent", config.ID)
			config.Status.State = configapi.ConfigurationStatus_PERSISTED
			if err := r.updateConfigurationStatus(ctx, config); err != nil {
				return controller.Result{}, err
			}
			return controller.Result{}, nil
		}
		return controller.Result{}, nil
	}

	// If the configuration is not SYNCHRONIZING, skip synchronization.
	if config.Status.State != configapi.ConfigurationStatus_SYNCHRONIZING {
		// If the configuration term is greater than the applied term, set the state to SYNCHRONIZING
		if config.Status.Mastership.Term > config.Status.Applied.Mastership.Term {
			log.Infof("Configuration '%s' mastership term has increased, synchronizing...", config.ID)
			config.Status.State = configapi.ConfigurationStatus_SYNCHRONIZING
			if err := r.updateConfigurationStatus(ctx, config); err != nil {
				return controller.Result{}, err
			}
			return controller.Result{}, nil
		}
		return controller.Result{}, nil
	}

	// If the master ID is not set, skip reconciliation.
	if config.Status.Mastership.Master == "" {
		log.Debugf("No master for target '%s'", config.TargetID)
		return controller.Result{}, nil
	}

	// If the applied index is 0, skip applying changes.
	if config.Status.Applied.Index == 0 {
		log.Infof("Skipping synchronization of Configuration '%s': no applied changes to synchronize", config.ID)
		config.Status.State = configapi.ConfigurationStatus_SYNCHRONIZED
		config.Status.Applied.Mastership.Master = config.Status.Mastership.Master
		config.Status.Applied.Mastership.Term = config.Status.Mastership.Term
		if err := r.updateConfigurationStatus(ctx, config); err != nil {
			return controller.Result{}, err
		}
		return controller.Result{}, nil
	}

	// If we've made it this far, we know there's a master relation.
	// Get the relation and check whether this node is the source
	relation, err := r.topo.Get(ctx, topoapi.ID(config.Status.Mastership.Master))
	if err != nil {
		if !errors.IsNotFound(err) {
			log.Errorf("Failed fetching master Relation '%s' from topo", config.Status.Mastership.Master, err)
			return controller.Result{}, err
		}
		log.Warnf("Master relation not found for target '%s'", config.TargetID)
		return controller.Result{}, nil
	}
	if relation.GetRelation().SrcEntityID != controllerutils.GetOnosConfigID() {
		log.Debugf("Not the master for target '%s'", config.TargetID)
		return controller.Result{}, nil
	}

	// Get the master connection
	conn, ok := r.conns.Get(ctx, gnmi.ConnID(relation.ID))
	if !ok {
		log.Warnf("Connection not found for target '%s'", config.TargetID)
		return controller.Result{}, nil
	}

	indexedPathValues := make(map[configapi.Index][]*configapi.PathValue)
	if config.Status.Applied.Values != nil {
		for _, appliedValue := range config.Status.Applied.Values {
			indexedPathValues[appliedValue.Index] = append(indexedPathValues[appliedValue.Index], appliedValue)
		}
	}
	log.Infof("Updating %d paths on target '%s'", len(indexedPathValues), config.TargetID)

	// Send the indexes in order. Each index becomes its own Set, and a failure
	// aborts the rest, so a random order would push a different subset of the
	// configuration on each attempt and make a failure hard to reproduce.
	for _, transactionIndex := range sortedIndexes(indexedPathValues) {
		pathValues := indexedPathValues[transactionIndex]
		// Create a gNMI set request
		log.Debugw("Creating Set request for changes in transaction", "TransactionIndex", transactionIndex)
		setRequest, err := utilsv2.PathValuesToGnmiChange(pathValues, config.TargetID)
		if err != nil {
			log.Errorf("Failed constructing SetRequest for Configuration '%s'", config.ID, err)
			return controller.Result{}, nil
		}

		// Add the master arbitration extension to provide concurrency control for multi-node controllers.
		setRequest.Extension = append(setRequest.Extension, &gnmi_ext.Extension{
			Ext: &gnmi_ext.Extension_MasterArbitration{
				MasterArbitration: &gnmi_ext.MasterArbitration{
					Role: &gnmi_ext.Role{
						Id: "onos-config",
					},
					ElectionId: &gnmi_ext.Uint128{
						Low: uint64(config.Status.Mastership.Term),
					},
				},
			},
		})

		// Execute the set request
		log.Debugf("Sending SetRequest %+v", setRequest)
		setResponse, err := conn.Set(ctx, setRequest)
		if err != nil {
			// The gNMI Set request can be denied if this master has been superseded by a master in a later term.
			// Rather than reverting to the STALE state now, wait for this node to see the mastership state change
			// to avoid flapping between states while the system converges.
			if errors.IsForbidden(err) {
				log.Warnf("Configuration '%s' mastership superseded for term %d", config.ID, config.Status.Mastership.Term)
				return controller.Result{}, nil
			}

			// A change the device will never accept must not be retried
			// forever: the configuration would stay in SYNCHRONIZING and
			// block every later change to this target. Retry while the cause
			// looks temporary, then give up and roll the index back so the
			// store matches what the device actually holds.
			attempts := r.recordSyncAttempt(config.ID, transactionIndex)
			if isTransientSyncFailure(err) && attempts < maxSyncAttempts {
				log.Warnf("Failed synchronizing index %d of Configuration '%s' (attempt %d/%d), retrying: %s",
					transactionIndex, config.ID, attempts, maxSyncAttempts, err.Error())
				return controller.Result{}, err
			}

			log.Errorf("Giving up on index %d of Configuration '%s' after %d attempts, rolling back: %s",
				transactionIndex, config.ID, attempts, err.Error())
			if rbErr := r.rollbackIndex(ctx, config, transactionIndex, err); rbErr != nil {
				return controller.Result{}, rbErr
			}
			// Requeue so the remaining indexes are synchronized now that the
			// failing one is out of the way.
			return controller.Result{Requeue: controller.NewID(config.ID)}, nil
		}
		log.Debugf("Received SetResponse %+v", setResponse)
	}
	r.clearSyncAttempt(config.ID)

	// Update the configuration state and path statuses
	log.Infof("Configuration '%s' synchronization complete", config.ID)
	config.Status.Applied.Mastership.Master = config.Status.Mastership.Master
	config.Status.Applied.Mastership.Term = config.Status.Mastership.Term
	config.Status.State = configapi.ConfigurationStatus_SYNCHRONIZED
	if err := r.updateConfigurationStatus(ctx, config); err != nil {
		return controller.Result{}, err
	}
	return controller.Result{}, nil
}

func (r *Reconciler) updateConfigurationStatus(ctx context.Context, configuration *configapi.Configuration) error {
	log.Debug(configuration.Status)
	err := r.configurations.UpdateStatus(ctx, configuration)
	if err != nil {
		if !errors.IsNotFound(err) && !errors.IsConflict(err) {
			log.Errorf("Failed updating Proposal '%s' status", configuration.ID, err)
			return err
		}
		log.Warnf("Write conflict updating Proposal '%s' status", configuration.ID, err)
		return nil
	}
	return nil
}

// sortedIndexes returns the transaction indexes in ascending order.
func sortedIndexes(values map[configapi.Index][]*configapi.PathValue) []configapi.Index {
	indexes := make([]configapi.Index, 0, len(values))
	for index := range values {
		indexes = append(indexes, index)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
	return indexes
}

// isTransientSyncFailure reports whether err may succeed on a later attempt.
//
// The southbound client converts gRPC status codes with errors.FromGRPC, so
// these predicates see the code the adapter returned. Anything not listed is
// treated as permanent: retrying a change the device has rejected on its
// merits only delays every later change to the same target.
func isTransientSyncFailure(err error) bool {
	switch {
	case errors.IsUnavailable(err), // the device or the adapter is unreachable
		errors.IsTimeout(err),  // the device accepted the session but did not answer
		errors.IsCanceled(err), // the attempt was cut short, not refused
		errors.IsConflict(err), // concurrent write, may clear on its own
		errors.IsInternal(err): // unclassified adapter fault, give it a chance
		return true
	default:
		// Invalid, NotFound, AlreadyExists, NotSupported, Unauthorized,
		// Forbidden: the device will answer the same way next time.
		return false
	}
}

// recordSyncAttempt increments and returns the failure count for an index.
// Switching to a different index resets the count, so progress on one change
// does not consume the budget of the next.
func (r *Reconciler) recordSyncAttempt(id configapi.ConfigurationID, index configapi.Index) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	attempt, ok := r.attempts[id]
	if !ok || attempt.index != index {
		attempt = &syncAttempt{index: index}
		r.attempts[id] = attempt
	}
	attempt.count++
	return attempt.count
}

// clearSyncAttempt drops the failure count after a successful synchronization.
func (r *Reconciler) clearSyncAttempt(id configapi.ConfigurationID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.attempts, id)
}

// rollbackIndex removes the values of a failed transaction index from the
// configuration and leaves SYNCHRONIZING.
//
// The store is the controller's record of what the device holds, so keeping
// values the device refused would make every later read report state that is
// not there. The adapter applies a Set atomically and NETCONF rolls back on
// error, so a failed index changed nothing on the device and dropping it
// restores agreement.
//
// The change the caller made is lost. That is deliberate: the alternative is
// retrying forever, which blocks the target. The reason is recorded on the
// proposal so the client can see which change was withdrawn and why.
func (r *Reconciler) rollbackIndex(ctx context.Context, config *configapi.Configuration, index configapi.Index, cause error) error {
	// Drop the values of the failed index so the store stops advertising state
	// the device does not hold.
	//
	// Known limitation: UpdateStatus persists only the paths present in this
	// map, so the removal is not durable and a restart reloads the values and
	// retries once more. Writing a tombstone instead would persist but then
	// gets pushed as an update to a non-leaf node, which the southbound
	// rejects, and the retry becomes a loop. Bounded repetition is the safer
	// of the two until the store layer can delete a path.
	rolled := 0
	for path, value := range config.Status.Applied.Values {
		if value.Index == index {
			delete(config.Status.Applied.Values, path)
			rolled++
		}
	}
	log.Warnf("Rolled back %d paths of index %d from Configuration '%s'", rolled, index, config.ID)

	// Leave SYNCHRONIZING so the proposal controller can apply later changes.
	config.Status.Applied.Mastership.Master = config.Status.Mastership.Master
	config.Status.Applied.Mastership.Term = config.Status.Mastership.Term
	config.Status.State = configapi.ConfigurationStatus_SYNCHRONIZED
	if err := r.updateConfigurationStatus(ctx, config); err != nil {
		return err
	}

	r.clearSyncAttempt(config.ID)
	r.recordSyncRollback(ctx, config.TargetID, index, cause)
	return nil
}

// recordSyncRollback writes the rollback cause to the proposal of the failed
// index, which is what surfaces it to clients watching transactions.
func (r *Reconciler) recordSyncRollback(ctx context.Context, targetID configapi.TargetID, index configapi.Index, cause error) {
	p, err := r.proposals.Get(ctx, proposalstore.NewID(targetID, index))
	if err != nil {
		if !errors.IsNotFound(err) {
			log.Warnf("Failed fetching Proposal %d of target '%s' to record rollback", index, targetID, err)
		}
		return
	}

	p.Status.Phases.Apply.State = configapi.ProposalApplyPhase_FAILED
	p.Status.Phases.Apply.Failure = &configapi.Failure{
		Type:        syncFailureType(cause),
		Description: fmt.Sprintf("%s%s", syncRollbackPrefix, cause.Error()),
	}
	if err := r.proposals.UpdateStatus(ctx, p); err != nil {
		log.Warnf("Failed recording rollback of Proposal %d for target '%s'", index, targetID, err)
	}
}

// syncFailureType maps a synchronization error to a Failure type.
func syncFailureType(err error) configapi.Failure_Type {
	switch {
	case errors.IsCanceled(err):
		return configapi.Failure_CANCELED
	case errors.IsNotFound(err):
		return configapi.Failure_NOT_FOUND
	case errors.IsAlreadyExists(err):
		return configapi.Failure_ALREADY_EXISTS
	case errors.IsUnauthorized(err):
		return configapi.Failure_UNAUTHORIZED
	case errors.IsForbidden(err):
		return configapi.Failure_FORBIDDEN
	case errors.IsConflict(err):
		return configapi.Failure_CONFLICT
	case errors.IsInvalid(err):
		return configapi.Failure_INVALID
	case errors.IsUnavailable(err):
		return configapi.Failure_UNAVAILABLE
	case errors.IsNotSupported(err):
		return configapi.Failure_NOT_SUPPORTED
	case errors.IsTimeout(err):
		return configapi.Failure_TIMEOUT
	case errors.IsInternal(err):
		return configapi.Failure_INTERNAL
	default:
		return configapi.Failure_UNKNOWN
	}
}
