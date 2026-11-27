package uplink

import "airview/internal/model"

// SplitUplinks separates a configured uplink list into the AVF/1 uplinks
// (Manager.Apply) and the Beast outputs (Manager.ApplyBeast) by
// model.Uplink.Type. Disabled entries are kept; the manager honours them.
func SplitUplinks(list []model.Uplink) (avfUps []model.Uplink, beast []BeastUplink) {
	for _, u := range list {
		if !u.IsBeast() {
			avfUps = append(avfUps, u)
			continue
		}
		beast = append(beast, BeastUplink{
			ID: u.ID, Host: u.Host, Port: u.Port, Format: u.Format, UUID: u.UUID,
			Inputs: append([]string(nil), u.Inputs...), ReduceMs: u.ReduceMs,
			NoTimestamps: !u.Timestamps, Disabled: u.Disabled,
		})
	}
	return avfUps, beast
}

// ApplyAll applies a configured uplink list of both types (SplitUplinks)
// and returns the Beast normalisation notes.
func (m *Manager) ApplyAll(list []model.Uplink) []string {
	avfUps, beast := SplitUplinks(list)
	m.Apply(avfUps)
	return m.ApplyBeast(beast)
}
