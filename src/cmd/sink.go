package cmd

import (
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"
	"github.com/PrPlanIT/HASteward/src/output/printer"
)

// printerSink adapts a *printer.Printer to the engine.StepSink interface.
// In JSONL mode it emits step events; in human mode it prints section headers.
type printerSink struct {
	p *printer.Printer
}

func newSink(p *printer.Printer) *printerSink {
	return &printerSink{p: p}
}

func (s *printerSink) Step(name, status string) {
	switch status {
	case "running":
		s.p.EmitEvent(model.NewEvent(model.EventStepStarted, s.p.Command, s.p.RunID).
			WithPhase(name).WithMessage(name + " started"))
		if s.p.IsHuman() {
			output.Section(name)
		}
	case "done":
		s.p.EmitEvent(model.NewEvent(model.EventStepComplete, s.p.Command, s.p.RunID).
			WithPhase(name).WithSuccess(true))
	}
}
