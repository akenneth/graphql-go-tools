package postprocess

import (
	"github.com/wundergraph/graphql-go-tools/v2/pkg/engine/resolve"
)

type mergeSameSourceFetches struct {
	disable bool
}

// to do: rename to UNION where applicable
func (m *mergeSameSourceFetches) ProcessFetchTree(root *resolve.FetchTreeNode) {
	if m.disable {
		return
	}

	fetchGroups := make(map[string][]*resolve.FetchTreeNode)

	// todo: consider dependencies when merging
	for _, node := range root.ChildNodes {
		// todo: does this need to dive down recursively?
		if node.Kind == resolve.FetchTreeNodeKindSingle {
			info := node.Item.Fetch.DataSourceInfo()
			key := info.ID // + string(node.Item.Fetch.(*resolve.SingleFetch).InputTemplate...)
			fetchGroups[key] = append(fetchGroups[key], node)
		}
	}

	var mergedNodes []*resolve.FetchTreeNode
	for _, group := range fetchGroups {
		if len(group) > 1 {
			mergedNode := mergeFetchNodes(group)
			mergedNodes = append(mergedNodes, mergedNode)
		} else {
			mergedNodes = append(mergedNodes, group[0])
		}
	}

	root.ChildNodes = mergedNodes
}

func mergeFetchNodes(nodes []*resolve.FetchTreeNode) *resolve.FetchTreeNode {
	mergedNode := resolve.Union(nodes[0])
	mergedNode.ChildNodes = append(mergedNode.ChildNodes, nodes[1:]...)
	return mergedNode
}
