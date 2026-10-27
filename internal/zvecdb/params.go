package zvecdb

import (
	"fmt"

	zvec "github.com/zvec-ai/zvec-go"
)

// queryTarget is a whole search or one half of a hybrid search. The engine
// gives both the same five "set the index's search params" methods without a
// common interface, so the fan-out happens here once.
type queryTarget struct {
	search *zvec.SearchQuery
	sub    *zvec.SubQuery
}

// applyQueryParams sets the index's search-time knobs from what the user typed,
// choosing the shape of the params from the index the field actually has — so
// a form can offer one "search params" box instead of one per index type.
//
// Nothing is applied when no knobs were given: the engine's own defaults are
// better than this plugin's guesses, and an `ef` of our choosing would quietly
// override a collection tuned by its owner.
//
// The returned function must be called when the query is done with. The engine
// takes ownership of the params it accepts, which the Go binding records by
// dropping its handle, so releasing is a no-op after a successful set and a
// free after a failed one.
func applyQueryParams(target queryTarget, index zvec.IndexType, values map[string]any) (func(), error) {
	if len(values) == 0 {
		return func() {}, nil
	}
	p := params(values)

	var (
		release func()
		err     error
	)
	switch index {
	case zvec.IndexTypeHNSW:
		built := zvec.NewHNSWQueryParams(
			p.intOr("ef", 100),
			float32(p.floatOr("radius", 0)),
			p.boolOr("isLinear", false),
			p.boolOr("isUsingRefiner", false),
		)
		if built == nil {
			return nil, allocFailed("HNSW")
		}
		release = built.Destroy
		err = target.setHNSW(built)
	case zvec.IndexTypeIVF:
		built := zvec.NewIVFQueryParams(
			p.intOr("nprobe", 8),
			p.boolOr("isUsingRefiner", false),
			float32(p.floatOr("scaleFactor", 0)),
		)
		if built == nil {
			return nil, allocFailed("IVF")
		}
		release = built.Destroy
		err = target.setIVF(built)
	case zvec.IndexTypeIVFRaBitQ:
		built := zvec.NewIVFRaBitQQueryParams(
			p.intOr("nprobe", 8),
			float32(p.floatOr("radius", 0)),
			p.boolOr("isLinear", false),
			p.boolOr("isUsingRefiner", false),
		)
		if built == nil {
			return nil, allocFailed("IVF-RaBitQ")
		}
		release = built.Destroy
		err = target.setIVFRaBitQ(built)
	case zvec.IndexTypeFlat:
		built := zvec.NewFlatQueryParams(
			p.boolOr("isUsingRefiner", false),
			float32(p.floatOr("scaleFactor", 0)),
		)
		if built == nil {
			return nil, allocFailed("flat")
		}
		release = built.Destroy
		err = target.setFlat(built)
	case zvec.IndexTypeDiskANN, zvec.IndexTypeVamana:
		built := zvec.NewDiskANNQueryParams(p.intOr("listSize", 100))
		if built == nil {
			return nil, allocFailed("DiskANN")
		}
		release = built.Destroy
		err = target.setDiskANN(built)
	default:
		// A field with no vector index takes no search params. Saying so beats
		// accepting them and searching as if they had been applied.
		return nil, fmt.Errorf("search params were given, but the field being searched has no vector index to apply them to (its index is %s): leave the search params empty", index)
	}

	if paramErr := p.err(); paramErr != nil {
		release()
		return nil, fmt.Errorf("search params: %w", paramErr)
	}
	if err != nil {
		release()
		return nil, fmt.Errorf("cannot apply the %s search params: %w", index, err)
	}
	return release, nil
}

func allocFailed(kind string) error {
	return fmt.Errorf("%w: cannot allocate %s query params", ErrLibrary, kind)
}

func (t queryTarget) setHNSW(p *zvec.HNSWQueryParams) error {
	if t.search != nil {
		return t.search.SetHNSWParams(p)
	}
	return t.sub.SetHNSWParams(p)
}

func (t queryTarget) setIVF(p *zvec.IVFQueryParams) error {
	if t.search != nil {
		return t.search.SetIVFParams(p)
	}
	return t.sub.SetIVFParams(p)
}

func (t queryTarget) setIVFRaBitQ(p *zvec.IVFRaBitQQueryParams) error {
	if t.search != nil {
		return t.search.SetIVFRaBitQParams(p)
	}
	return t.sub.SetIVFRaBitQParams(p)
}

func (t queryTarget) setFlat(p *zvec.FlatQueryParams) error {
	if t.search != nil {
		return t.search.SetFlatParams(p)
	}
	return t.sub.SetFlatParams(p)
}

func (t queryTarget) setDiskANN(p *zvec.DiskANNQueryParams) error {
	if t.search != nil {
		return t.search.SetDiskANNParams(p)
	}
	return t.sub.SetDiskANNParams(p)
}
