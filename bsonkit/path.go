package bsonkit

import (
	"strconv"
	"strings"
	"sync"
)

// PathEnd is returned by X if the end of the path has been reached.
var PathEnd = "\x00"

// ReducePath will reduce the path by one segment. It will return PathEnd if
// there are no more segments.
func ReducePath(path string) string {
	// get next dot
	i := strings.IndexByte(path, '.')
	if i >= 0 {
		return path[i+1:]
	}

	return PathEnd
}

// ReducePathReverse will reduce the path by one segment from the back. It will
// return PathEnd if there are no more segments.
func ReducePathReverse(path string) string {
	// get last dot
	i := strings.LastIndexByte(path, '.')
	if i >= 0 {
		return path[:i]
	}

	return PathEnd
}

// PathSegment will return the first segment of the path.
func PathSegment(path string) string {
	// get first dot
	i := strings.IndexByte(path, '.')
	if i >= 0 {
		return path[:i]
	}

	return path
}

// PathSegmentReverse will return the last segment of the path.
func PathSegmentReverse(path string) string {
	// get first dot
	i := strings.LastIndexByte(path, '.')
	if i >= 0 {
		return path[i+1:]
	}

	return path
}

// ParseIndex will attempt to parse the provided string as an index.
func ParseIndex(str string) (int, bool) {
	// check if strings begins with a number
	if len(str) == 0 || str[0] < '0' || str[0] > '9' {
		return 0, false
	}

	// parse number
	index, err := strconv.Atoi(str)
	if err != nil {
		return 0, false
	}

	return index, true
}

var pathNodePool = sync.Pool{
	New: func() interface{} {
		return PathNode{}
	},
}

// PathNode is a node of a path tree.
type PathNode map[string]interface{}

// NewPathNode will return a new path node.
func NewPathNode() PathNode {
	return pathNodePool.Get().(PathNode)
}

// Store will set the specified value and return the previous stored value.
func (n PathNode) Store(value interface{}) interface{} {
	prev := n[PathEnd]
	n[PathEnd] = value
	return prev
}

// Load will return the currently stored value.
func (n PathNode) Load() interface{} {
	return n[PathEnd]
}

// Append will traverse the path and append missing nodes.
func (n PathNode) Append(path string) PathNode {
	// set value on leaf
	if path == PathEnd {
		return n
	}

	// get segment
	segment := PathSegment(path)

	// get child
	child, ok := n[segment].(PathNode)
	if !ok {
		child = NewPathNode()
		n[segment] = child
	}

	// descend
	return child.Append(ReducePath(path))
}

// Lookup will traverse the path and return the last node. If the returned path
// is PathEnd the returned node is the final node.
func (n PathNode) Lookup(path string) (PathNode, string) {
	// return value from leaf
	if path == PathEnd {
		return n, PathEnd
	}

	// get child
	child, ok := n[PathSegment(path)]
	if !ok {
		return n, path
	}

	// descend
	return child.(PathNode).Lookup(ReducePath(path))
}

// Recycle will clear the node and its children.
func (n PathNode) Recycle() {
	// descend
	for key, value := range n {
		if key != PathEnd {
			value.(PathNode).Recycle()
		}
	}

	// clear
	for key := range n {
		delete(n, key)
	}

	// recycle
	pathNodePool.Put(n)
}
