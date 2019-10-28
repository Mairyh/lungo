package lungo

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/256dpi/lungo/bsonkit"
	"github.com/256dpi/lungo/mongokit"
)

// TODO: Combine ListDatabases(), ListCollections(), NumDocuments() into Info().

// Result is returned by some engine operations.
type Result struct {
	// The list of matched documents.
	Matched bsonkit.List

	// The list of inserted, replace or updated documents.
	Modified bsonkit.List

	// The upserted document.
	Upserted bsonkit.Doc

	// The errors that occurred during the operation.
	Errors []error
}

// Options is used to configure an engine.
type Options struct {
	// The store used by the engine to load and store the dataset.
	Store Store
}

// Engine manages the dataset loaded from a store and provides the various
// MongoDB style CRUD operations.
type Engine struct {
	store   Store
	dataset *Dataset
	mutex   sync.Mutex
}

// CreateEngine will create and return an engine with a loaded dataset from the
// store.
func CreateEngine(opts Options) (*Engine, error) {
	// create engine
	e := &Engine{
		store: opts.Store,
	}

	// load dataset
	data, err := e.store.Load()
	if err != nil {
		return nil, err
	}

	// set dataset
	e.dataset = data

	return e, nil
}

// Find will query documents from a namespace. Sort, skip and limit may be
// supplied to modify the result. The returned results will contain the matched
// list of documents.
func (e *Engine) Find(handle Handle, query, sort bsonkit.Doc, skip, limit int) (*Result, error) {
	// acquire mutex
	e.mutex.Lock()
	defer e.mutex.Unlock()

	// check namespace
	if e.dataset.Namespaces[handle] == nil {
		return &Result{}, nil
	}

	// get documents
	list := e.dataset.Namespaces[handle].Documents.List

	// sort documents
	var err error
	if sort != nil && len(*sort) > 0 {
		list, err = mongokit.Sort(list, sort)
		if err != nil {
			return nil, err
		}
	}

	// apply skip
	if skip > len(list) {
		list = nil
	} else {
		list = list[skip:]
	}

	// filter documents
	list, err = mongokit.Filter(list, query, limit)
	if err != nil {
		return nil, err
	}

	return &Result{Matched: list}, nil
}

// Insert will insert the specified documents into the namespace. The engine
// will automatically generate an object id per document if it is missing. If
// ordered ist enabled the operation is aborted on the first error and the
// result returned. Otherwise, the engine will try to insert all documents. The
// returned results will contain the inserted documents and potential errors.
func (e *Engine) Insert(handle Handle, list bsonkit.List, ordered bool) (*Result, error) {
	// acquire mutex
	e.mutex.Lock()
	defer e.mutex.Unlock()

	// clone list
	list = bsonkit.CloneList(list)

	// ensure ids
	for _, doc := range list {
		// ensure object id
		if bsonkit.Get(doc, "_id") == bsonkit.Missing {
			err := bsonkit.Put(doc, "_id", primitive.NewObjectID(), true)
			if err != nil {
				return nil, err
			}
		}
	}

	// clone dataset
	clone := e.dataset.Clone()

	// create or clone namespace
	var namespace *Namespace
	if clone.Namespaces[handle] == nil {
		namespace = NewNamespace()
		clone.Namespaces[handle] = namespace
	} else {
		namespace = clone.Namespaces[handle].Clone()
		clone.Namespaces[handle] = namespace
	}

	// prepare result
	result := &Result{}

	// insert documents
	for _, doc := range list {
		// list uniqueness pre-check
		if _, ok := namespace.Documents.Index[doc]; ok {
			result.Errors = append(result.Errors, fmt.Errorf("duplicate document in namespace %q", handle.String()))
			if ordered {
				break
			} else {
				continue
			}
		}

		// add document to all indexes
		var duplicateIndex string
		for name, index := range namespace.Indexes {
			if !index.Add(doc) {
				duplicateIndex = name
			}
		}
		if duplicateIndex != "" {
			result.Errors = append(result.Errors, fmt.Errorf("duplicate document for index %q", duplicateIndex))
			if ordered {
				break
			} else {
				continue
			}
		}

		// add document
		namespace.Documents.Add(doc)

		// add to list
		result.Modified = append(result.Modified, doc)
	}

	// check if documents have been inserted
	if len(result.Modified) > 0 {
		// write dataset
		err := e.store.Store(clone)
		if err != nil {
			return nil, err
		}

		// set new dataset
		e.dataset = clone
	}

	return result, nil
}

// Replace will replace the first matching document with the specified
// replacement document. If upsert is enabled, it will insert the replacement
// document if it is missing. The returned result will contain the matched
// and modified or upserted document.
func (e *Engine) Replace(handle Handle, query, sort, repl bsonkit.Doc, upsert bool) (*Result, error) {
	// acquire mutex
	e.mutex.Lock()
	defer e.mutex.Unlock()

	// clone replacement
	repl = bsonkit.Clone(repl)

	// get documents
	var list bsonkit.List
	if e.dataset.Namespaces[handle] != nil {
		list = e.dataset.Namespaces[handle].Documents.List
	}

	// sort documents
	var err error
	if sort != nil && len(*sort) > 0 {
		list, err = mongokit.Sort(list, sort)
		if err != nil {
			return nil, err
		}
	}

	// filter documents
	list, err = mongokit.Filter(list, query, 1)
	if err != nil {
		return nil, err
	}

	// check list
	if len(list) == 0 {
		// handle upsert
		if upsert {
			return e.upsert(handle, query, repl, nil)
		}

		return &Result{}, nil
	}

	// set missing id or check existing id
	replID := bsonkit.Get(repl, "_id")
	if replID == bsonkit.Missing {
		err = bsonkit.Put(repl, "_id", bsonkit.Get(list[0], "_id"), true)
		if err != nil {
			return nil, err
		}
	} else if replID != bsonkit.Get(list[0], "_id") {
		return nil, fmt.Errorf("document _id is immutable")
	}

	// clone dataset
	clone := e.dataset.Clone()

	// clone namespace
	namespace := clone.Namespaces[handle].Clone()
	clone.Namespaces[handle] = namespace

	// update indexes
	for name, index := range namespace.Indexes {
		// remove old document
		index.Remove(list[0])

		// add replacement
		if !index.Add(repl) {
			return nil, fmt.Errorf("duplicate document for index %q", name)
		}
	}

	// replace document
	namespace.Documents.Replace(list[0], repl)

	// write dataset
	err = e.store.Store(clone)
	if err != nil {
		return nil, err
	}

	// set new dataset
	e.dataset = clone

	return &Result{
		Matched:  list,
		Modified: bsonkit.List{repl},
	}, nil
}

// Update will apply the update to all matching document. Sort, skip and limit
// may be supplied to modify the result. If upsert is enabled, it will extract
// constant parts of the query and apply the update and insert the document if
// it is missing. The returned result will contain the matched and modified or
// upserted document.
func (e *Engine) Update(handle Handle, query, sort, update bsonkit.Doc, limit int, upsert bool) (*Result, error) {
	// acquire mutex
	e.mutex.Lock()
	defer e.mutex.Unlock()

	// get documents
	var list bsonkit.List
	if e.dataset.Namespaces[handle] != nil {
		list = e.dataset.Namespaces[handle].Documents.List
	}

	// sort documents
	var err error
	if sort != nil && len(*sort) > 0 {
		list, err = mongokit.Sort(list, sort)
		if err != nil {
			return nil, err
		}
	}

	// filter documents
	list, err = mongokit.Filter(list, query, limit)
	if err != nil {
		return nil, err
	}

	// check list
	if len(list) == 0 {
		// handle upsert
		if upsert {
			return e.upsert(handle, query, nil, update)
		}

		return &Result{}, nil
	}

	// clone documents
	newList := bsonkit.CloneList(list)

	// update documents
	err = mongokit.Update(newList, update, false)
	if err != nil {
		return nil, err
	}

	// check ids
	for i, doc := range newList {
		if bsonkit.Get(doc, "_id") != bsonkit.Get(list[i], "_id") {
			return nil, fmt.Errorf("document _id is immutable")
		}
	}

	// clone dataset
	clone := e.dataset.Clone()

	// clone namespace
	namespace := clone.Namespaces[handle].Clone()
	clone.Namespaces[handle] = namespace

	// remove old docs from indexes
	for _, doc := range list {
		for _, index := range namespace.Indexes {
			index.Remove(doc)
		}
	}

	// add new docs to indexes
	for _, doc := range newList {
		for name, index := range namespace.Indexes {
			if !index.Add(doc) {
				return nil, fmt.Errorf("duplicate document for index %q", name)
			}
		}
	}

	// replace documents
	for i, doc := range newList {
		namespace.Documents.Replace(list[i], doc)
	}

	// write dataset
	err = e.store.Store(clone)
	if err != nil {
		return nil, err
	}

	// set new dataset
	e.dataset = clone

	return &Result{
		Matched:  list,
		Modified: newList,
	}, nil
}

func (e *Engine) upsert(handle Handle, query, repl, update bsonkit.Doc) (*Result, error) {
	// extract query
	doc, err := mongokit.Extract(query)
	if err != nil {
		return nil, err
	}

	// set replacement if present
	if repl != nil {
		// get ids
		queryID := bsonkit.Get(doc, "_id")
		replID := bsonkit.Get(repl, "_id")

		// check ids
		if queryID != bsonkit.Missing && replID != bsonkit.Missing {
			if bsonkit.Compare(replID, queryID) != 0 {
				return nil, fmt.Errorf("query _id and replacement _id must match")
			}
		}

		// clone replacement
		doc = bsonkit.Clone(repl)

		// add repl or query id if present
		if replID != bsonkit.Missing {
			err = bsonkit.Put(doc, "_id", replID, true)
			if err != nil {
				return nil, err
			}
		} else if queryID != bsonkit.Missing {
			err = bsonkit.Put(doc, "_id", queryID, true)
			if err != nil {
				return nil, err
			}
		}
	}

	// apply update if present
	if update != nil {
		err = mongokit.Apply(doc, update, true)
		if err != nil {
			return nil, err
		}
	}

	// generate object id if missing
	if bsonkit.Get(doc, "_id") == bsonkit.Missing {
		err := bsonkit.Put(doc, "_id", primitive.NewObjectID(), true)
		if err != nil {
			return nil, err
		}
	}

	// clone dataset
	clone := e.dataset.Clone()

	// create or clone namespace
	var namespace *Namespace
	if clone.Namespaces[handle] == nil {
		namespace = NewNamespace()
		clone.Namespaces[handle] = namespace
	} else {
		namespace = clone.Namespaces[handle].Clone()
		clone.Namespaces[handle] = namespace
	}

	// add document to indexes
	for name, index := range namespace.Indexes {
		if !index.Add(doc) {
			return nil, fmt.Errorf("duplicate document for index %q", name)
		}
	}

	// add document
	namespace.Documents.Add(doc)

	// write dataset
	err = e.store.Store(clone)
	if err != nil {
		return nil, err
	}

	// set new dataset
	e.dataset = clone

	return &Result{
		Upserted: doc,
	}, nil
}

// Delete will remove all matching documents from the namespace. Sort, skip and
// limit may be supplied to modify the result. The returned result will contain
// the matched documents.
func (e *Engine) Delete(handle Handle, query, sort bsonkit.Doc, limit int) (*Result, error) {
	// acquire mutex
	e.mutex.Lock()
	defer e.mutex.Unlock()

	// check namespace
	if e.dataset.Namespaces[handle] == nil {
		return &Result{}, nil
	}

	// get documents
	list := e.dataset.Namespaces[handle].Documents.List

	// sort documents
	var err error
	if sort != nil && len(*sort) > 0 {
		list, err = mongokit.Sort(list, sort)
		if err != nil {
			return nil, err
		}
	}

	// filter documents
	list, err = mongokit.Filter(list, query, limit)
	if err != nil {
		return nil, err
	}

	// clone dataset
	clone := e.dataset.Clone()

	// clone namespace
	namespace := clone.Namespaces[handle].Clone()
	clone.Namespaces[handle] = namespace

	// remove documents
	for _, doc := range list {
		namespace.Documents.Remove(doc)
	}

	// update indexes
	for _, doc := range list {
		for _, index := range namespace.Indexes {
			index.Remove(doc)
		}
	}

	// write dataset
	err = e.store.Store(clone)
	if err != nil {
		return nil, err
	}

	// set new dataset
	e.dataset = clone

	return &Result{Matched: list}, nil
}

// Drop will return the namespace with the specified handle from the dataset.
// If the second part of the handle is empty, it will drop all namespaces
// matching the first part.
func (e *Engine) Drop(handle Handle) error {
	// acquire mutex
	e.mutex.Lock()
	defer e.mutex.Unlock()

	// clone dataset
	clone := e.dataset.Clone()

	// drop all matching namespaces
	for ns := range clone.Namespaces {
		if ns == handle || handle[1] == "" && ns[0] == handle[0] {
			delete(clone.Namespaces, ns)
		}
	}

	// write dataset
	err := e.store.Store(clone)
	if err != nil {
		return err
	}

	// set new dataset
	e.dataset = clone

	return nil
}

// ListDatabases will return a list of all databases in the dataset.
func (e *Engine) ListDatabases(query bsonkit.Doc) (bsonkit.List, error) {
	// acquire mutex
	e.mutex.Lock()
	defer e.mutex.Unlock()

	// sort namespaces
	sort := map[string][]*Namespace{}
	for ns, namespace := range e.dataset.Namespaces {
		sort[ns[0]] = append(sort[ns[0]], namespace)
	}

	// prepare list
	var list bsonkit.List
	for name, nss := range sort {
		// check emptiness
		empty := true
		for _, ns := range nss {
			if len(ns.Documents.List) > 0 {
				empty = false
			}
		}

		// add specification
		list = append(list, &bson.D{
			bson.E{Key: "name", Value: name},
			bson.E{Key: "sizeOnDisk", Value: 0},
			bson.E{Key: "empty", Value: empty},
		})
	}

	// filter list
	list, err := mongokit.Filter(list, query, 0)
	if err != nil {
		return nil, err
	}

	return list, nil
}

// ListCollections will return a list of all collections in the specified db.
func (e *Engine) ListCollections(db string, query bsonkit.Doc) (bsonkit.List, error) {
	// acquire mutex
	e.mutex.Lock()
	defer e.mutex.Unlock()

	// prepare list
	list := make(bsonkit.List, 0, len(e.dataset.Namespaces))

	// add documents
	for ns := range e.dataset.Namespaces {
		if ns[0] == db {
			list = append(list, &bson.D{
				bson.E{Key: "name", Value: ns[1]},
				bson.E{Key: "type", Value: "collection"},
				bson.E{Key: "options", Value: bson.D{}},
				bson.E{Key: "info", Value: bson.D{
					bson.E{Key: "uuid", Value: ns.String()},
					bson.E{Key: "readOnly", Value: false},
				}},
				bson.E{Key: "idIndex", Value: bson.D{
					bson.E{Key: "v", Value: 2},
					bson.E{Key: "key", Value: bson.D{
						bson.E{Key: "_id", Value: 1},
					}},
					bson.E{Key: "name", Value: "_id_"},
					bson.E{Key: "namespace", Value: ns.String()},
				}},
			})
		}
	}

	// filter list
	list, err := mongokit.Filter(list, query, 0)
	if err != nil {
		return nil, err
	}

	return list, nil
}

// NumDocuments will return the number of documents in the specified namespace.
func (e *Engine) NumDocuments(handle Handle) int {
	// acquire mutex
	e.mutex.Lock()
	defer e.mutex.Unlock()

	// check namespace
	namespace, ok := e.dataset.Namespaces[handle]
	if !ok {
		return 0
	}

	return len(namespace.Documents.List)
}

// ListIndexes will return a list of indexes in the specified namespace.
func (e *Engine) ListIndexes(handle Handle) (bsonkit.List, error) {
	// acquire mutex
	e.mutex.Lock()
	defer e.mutex.Unlock()

	// check namespace
	if e.dataset.Namespaces[handle] == nil {
		return nil, fmt.Errorf("missing namespace %q", handle.String())
	}

	// get namespace
	namespace := e.dataset.Namespaces[handle]

	// prepare list
	var list bsonkit.List
	for name, index := range namespace.Indexes {
		// prepare key
		var key bson.D
		for _, column := range index.Columns {
			// get direction
			direction := 1
			if column.Reverse {
				direction = -1
			}

			// add element
			key = append(key, bson.E{
				Key:   column.Path,
				Value: direction,
			})
		}

		// create spec
		spec := bson.D{
			bson.E{Key: "v", Value: 2},
			bson.E{Key: "key", Value: key},
			bson.E{Key: "name", Value: name},
			bson.E{Key: "ns", Value: handle.String()},
		}

		// add uniqueness
		if index.Unique && name != "_id_" {
			spec = append(spec, bson.E{Key: "unique", Value: true})
		}

		// add specification
		list = append(list, &spec)
	}

	// sort list
	bsonkit.Sort(list, []bsonkit.Column{
		{Path: "name"},
	})

	return list, nil
}

// CreateIndex will create the specified index in the specified namespace.
func (e *Engine) CreateIndex(handle Handle, keys bsonkit.Doc, name string, unique bool) (string, error) {
	// acquire mutex
	e.mutex.Lock()
	defer e.mutex.Unlock()

	// get columns
	columns, err := mongokit.Columns(keys)
	if err != nil {
		return "", err
	}

	// generate name if missing
	if name == "" {
		segments := make([]string, 0, len(columns)*2)
		for _, column := range columns {
			var dir = 1
			if column.Reverse {
				dir = -1
			}
			segments = append(segments, column.Path, strconv.Itoa(dir))
		}
		name = strings.Join(segments, "_")
	}

	// clone dataset
	clone := e.dataset.Clone()

	// TODO: Prevent other indexes from being cloned?

	// create or clone namespace
	var namespace *Namespace
	if clone.Namespaces[handle] == nil {
		namespace = NewNamespace()
		clone.Namespaces[handle] = namespace
	} else {
		namespace = clone.Namespaces[handle].Clone()
		clone.Namespaces[handle] = namespace
	}

	// create index
	index := bsonkit.NewIndex(unique, columns)
	namespace.Indexes[name] = index

	// fill index
	for _, doc := range namespace.Documents.List {
		if !index.Add(doc) {
			return "", fmt.Errorf("duplicate document for index %q", name)
		}
	}

	// write dataset
	err = e.store.Store(clone)
	if err != nil {
		return "", err
	}

	// set new dataset
	e.dataset = clone

	return name, nil
}

// DropIndex will drop the specified index in the specified namespace.
func (e *Engine) DropIndex(handle Handle, name string) error {
	// acquire mutex
	e.mutex.Lock()
	defer e.mutex.Unlock()

	// check namespace
	if e.dataset.Namespaces[handle] == nil {
		return fmt.Errorf("missing namespace %q", handle.String())
	}

	// clone dataset
	clone := e.dataset.Clone()

	// clone namespace
	namespace := clone.Namespaces[handle].Clone()
	clone.Namespaces[handle] = namespace

	// delete single index
	if name != "*" {
		// check existence
		if _, ok := namespace.Indexes[name]; !ok {
			return fmt.Errorf("missing index %q", handle.String())
		}

		// drop index
		delete(namespace.Indexes, name)
	}

	// delete all indexes
	if name == "*" {
		for name := range namespace.Indexes {
			if name != "_id_" {
				delete(namespace.Indexes, name)
			}
		}
	}

	// write dataset
	err := e.store.Store(clone)
	if err != nil {
		return err
	}

	// set new dataset
	e.dataset = clone

	return nil
}
