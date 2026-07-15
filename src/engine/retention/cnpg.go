package retention

import (
	"context"
	"fmt"

	"github.com/PrPlanIT/HASteward/src/engine/provider"
	"github.com/PrPlanIT/HASteward/src/output/model"
)

func init() {
	Register("cnpg", func(p provider.EngineProvider) (Retainer, error) {
		cp, ok := p.(*provider.CNPGProvider)
		if !ok {
			return nil, fmt.Errorf("expected *provider.CNPGProvider, got %T", p)
		}
		return &cnpgRetainer{p: cp}, nil
	})
}

type cnpgRetainer struct {
	p *provider.CNPGProvider
}

func (r *cnpgRetainer) Name() string { return "cnpg" }

func (r *cnpgRetainer) Prune(ctx context.Context, opts PruneOptions) (*model.PruneResult, error) {
	return runPrune(ctx, r.p.Config(), r.Name(), opts)
}
