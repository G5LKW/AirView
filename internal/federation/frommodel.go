package federation

import (
	"encoding/json"

	"airview/internal/model"
)

// FromModel converts the config file block aggregator.federation
// (model.AggFederation, the same JSON shape) into a Config.
func FromModel(m model.AggFederation) Config {
	var c Config
	b, err := json.Marshal(m)
	if err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}
