package postprocess

import (
	"slices"

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

	// todo: does this need to dive down recursively?
	for i := 0; i < len(root.ChildNodes); i++ {
		// union single fetches only for now
		if !(root.ChildNodes[i].Kind == resolve.FetchTreeNodeKindSingle) {
			continue
		}

		providedFetchIDs := resolveProvidedFetchIDs(root.ChildNodes[:i])
		union := resolve.Union(root.ChildNodes[i])
		sourceId := root.ChildNodes[i].Item.Fetch.DataSourceInfo().ID

		for j := i + 1; j < len(root.ChildNodes); j++ {
			// union single fetches only for now
			if !(root.ChildNodes[j].Kind == resolve.FetchTreeNodeKindSingle) {
				continue
			}

			currentNodeSourceId := root.ChildNodes[j].Item.Fetch.DataSourceInfo().ID
			if m.dependenciesCanBeProvided(root.ChildNodes[j], providedFetchIDs) && sourceId == currentNodeSourceId {
				union.ChildNodes = append(union.ChildNodes, root.ChildNodes[j])
				root.ChildNodes = append(root.ChildNodes[:j], root.ChildNodes[j+1:]...)
				j--
			}
		}
		if len(union.ChildNodes) > 1 {
			root.ChildNodes[i] = union
		}
	}
}

func (c *mergeSameSourceFetches) dependenciesCanBeProvided(node *resolve.FetchTreeNode, providedFetchIDs []int) bool {
	deps := node.Item.Fetch.Dependencies()
	for _, dep := range deps.DependsOnFetchIDs {
		if !slices.Contains(providedFetchIDs, dep) {
			return false
		}
	}
	return true
}
