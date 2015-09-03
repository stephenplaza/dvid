// +build !clustered,!gcloud

package storage

import (
	"fmt"
	"strings"

	"github.com/janelia-flyem/dvid/dvid"
)

var manager managerT

// managerT should be implemented for each type of storage implementation (local, clustered, gcloud)
// and it should fulfill a storage.Manager interface.
type managerT struct {
	// True if Setupmanager and SetupTiers have been called.
	setup bool

	// Tiers
	metadata  MetaDataStorer
	mutable   MutableStorer
	immutable ImmutableStorer

	// Cached type-asserted interfaces
	graphEngine Engine
	graphDB     GraphDB
	graphSetter GraphSetter
	graphGetter GraphGetter

	enginesAvail []string
}

func MetaDataStore() (MetaDataStorer, error) {
	if !manager.setup {
		return nil, fmt.Errorf("Key-value store not initialized before requesting MetaDataStore")
	}
	return manager.metadata, nil
}

func MutableStore() (MutableStorer, error) {
	if !manager.setup {
		return nil, fmt.Errorf("Key-value store not initialized before requesting MutableStore")
	}
	return manager.mutable, nil
}

func ImmutableStore() (ImmutableStorer, error) {
	if !manager.setup {
		return nil, fmt.Errorf("Key-value store not initialized before requesting ImmutableStorer")
	}
	return manager.immutable, nil
}

func GraphStore() (GraphDB, error) {
	if !manager.setup {
		return nil, fmt.Errorf("Graph DB not initialized before requesting it")
	}
	return manager.graphDB, nil
}

// EnginesAvailable returns a description of the available storage engines.
func EnginesAvailable() string {
	return strings.Join(manager.enginesAvail, "; ")
}

// Shutdown handles any storage-specific shutdown procedures.
func Shutdown() {
	// Place to be put any storage engine shutdown code.
}

// Initialize the storage systems given a configuration, path to datastore.  Unlike cluster
// and google cloud storage systems, which get initialized on DVID start using init(), the
// local storage system waits until it receives a path and configuration data from a
// "serve" command.
func Initialize(kvEngine Engine, description string) error {
	kvDB, ok := kvEngine.(OrderedKeyValueDB)
	if !ok {
		return fmt.Errorf("Database %q is not a valid ordered key-value database", kvEngine.String())
	}

	var err error
	manager.graphEngine, err = NewGraphStore(kvDB)
	if err != nil {
		return err
	}
	manager.graphDB, ok = manager.graphEngine.(GraphDB)
	if !ok {
		return fmt.Errorf("Database %q cannot support a graph database", kvEngine.String())
	}
	manager.graphSetter, ok = manager.graphEngine.(GraphSetter)
	if !ok {
		return fmt.Errorf("Database %q cannot support a graph setter", kvEngine.String())
	}
	manager.graphGetter, ok = manager.graphEngine.(GraphGetter)
	if !ok {
		return fmt.Errorf("Database %q cannot support a graph getter", kvEngine.String())
	}

	// Setup the three tiers of storage.  In the case of a single local server with
	// embedded storage engines, it's simpler because we don't worry about cross-process
	// synchronization.
	manager.metadata = kvDB
	manager.mutable = kvDB
	manager.immutable = kvDB

	manager.enginesAvail = append(manager.enginesAvail, description)

	manager.setup = true
	return nil
}

// DeleteDataInstance removes a data instance across all versions and tiers of storage.
func DeleteDataInstance(data dvid.Data) error {
	if !manager.setup {
		return fmt.Errorf("Can't delete data instance %q before storage manager is initialized", data.DataName())
	}

	// Determine all database tiers that are distinct.
	dbs := []OrderedKeyValueDB{manager.mutable}
	if manager.mutable != manager.immutable {
		dbs = append(dbs, manager.immutable)
	}

	// For each storage tier, remove all key-values with the given instance id.
	dvid.Infof("Starting delete of instance %d: name %q, type %s\n", data.InstanceID(), data.DataName(), data.TypeName())
	ctx := NewDataContext(data, 0)
	for _, db := range dbs {
		if err := db.DeleteAll(ctx, true); err != nil {
			return err
		}
	}
	return nil
}
