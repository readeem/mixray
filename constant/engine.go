package constant

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Engine string

const (
	EngineXray   Engine = "xray"
	EngineMihomo Engine = "mihomo"
)

func ParseEngine(raw string) (Engine, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(EngineXray):
		return EngineXray, nil
	case string(EngineMihomo):
		return EngineMihomo, nil
	default:
		return "", fmt.Errorf("unsupported engine %q (expected xray or mihomo)", raw)
	}
}

func (e Engine) String() string {
	if e == "" {
		return string(EngineXray)
	}
	return string(e)
}

func (e *Engine) UnmarshalYAML(unmarshal func(any) error) error {
	var raw string
	if err := unmarshal(&raw); err != nil {
		return err
	}
	parsed, err := ParseEngine(raw)
	if err != nil {
		return err
	}
	*e = parsed
	return nil
}

func (e Engine) MarshalYAML() (any, error) {
	return e.String(), nil
}

func (e *Engine) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	parsed, err := ParseEngine(raw)
	if err != nil {
		return err
	}
	*e = parsed
	return nil
}

func (e Engine) MarshalJSON() ([]byte, error) {
	return json.Marshal(e.String())
}
