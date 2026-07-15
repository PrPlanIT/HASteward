package retention

import (
	"context"
	"fmt"

	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/output/model"
)

func init() {
	Register("galera", func(p provider.EngineProvider) (Retainer, error) {
		gp, ok := p.(*provider.GaleraProvider)
		if !ok {
			return nil, fmt.Errorf("expected *provider.GaleraProvider, got %T", p)
		}
		return &galeraRetainer{p: gp}, nil
	})
}

type galeraRetainer struct {
	p *provider.GaleraProvider
}

func (r *galeraRetainer) Name() string { return "galera" }

func (r *galeraRetainer) Prune(ctx context.Context, opts PruneOptions) (*model.PruneResult, error) {
	return runPrune(ctx, r.p.Config(), r.Name(), opts)
}
