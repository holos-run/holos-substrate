package quay

type quayMutation struct {
	Mutated bool
	// HealedDrift carries an explicit mutation-site drift signal. The current
	// quay reconcilers set it conservatively only when the current generation is
	// already Ready, which matches stampMutation's reason fallback today; keeping
	// it distinct preserves the ADR-22 shape for future status mirrors that can
	// detect out-of-band drift during a spec-change reconcile.
	HealedDrift bool
}

func (m quayMutation) or(other quayMutation) quayMutation {
	return quayMutation{
		Mutated:     m.Mutated || other.Mutated,
		HealedDrift: m.HealedDrift || other.HealedDrift,
	}
}
