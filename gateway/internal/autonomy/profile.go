package autonomy

import (
	"errors"
	"fmt"
	"math"
)

// 心智档案（设计 §7.2 / §7.3）：候选的加权打分、可选的内部维度与阈值。
//
// 默认值复现原有排序关系：兴趣和新颖各占一份，成本权重 0；维度全部关闭，
// 打开之后才参与简报和阈值事件，不是人格、权限或真实生理状态的替代品。
const (
	PresetBalanced = "balanced"
	PresetCurious  = "curious"
	PresetFocused  = "focused"
	PresetCustom   = "custom"

	EventThreshold = "mind_threshold_crossed"
)

// DimensionNames 是首版内置的四个维度。
var DimensionNames = []string{"curiosity", "social_drive", "focus", "energy"}

// ScoreWeights 是候选选择分的权重：score = wi*(interest-1)/4 + wn*(novelty-1)/4 - wc*min(cost/scale,1)。
type ScoreWeights struct {
	Interest  float64 `json:"interest"`   // 兴趣权重 0–5
	Novelty   float64 `json:"novelty"`    // 新颖权重 0–5
	Cost      float64 `json:"cost"`       // 成本权重 0–5
	CostScale int     `json:"cost_scale"` // 成本参照分钟数 1–480
}

// Dimension 是一个可选的内部维度：0–100 的当前值、基线和范围，可以带一个阈值。
type Dimension struct {
	Enabled   bool    `json:"enabled"`
	Value     float64 `json:"value"`
	Baseline  float64 `json:"baseline"`
	Min       float64 `json:"min"`
	Max       float64 `json:"max"`
	Drift     float64 `json:"drift,omitempty"`     // 每次结算向基线靠多少（0–10）
	Threshold float64 `json:"threshold,omitempty"` // 越过它发一条 mind.threshold_crossed（0 表示不设）
	// Armed 是阈值迟滞的另一半：报过一次就不再报，等值回落到阈值以下 hysteresis 才重新装上
	Armed *bool `json:"armed,omitempty"`
}

// MindProfile 是保存下来的一份档案；Preset 只说明它是从哪个预设来的，改过就是 custom。
type MindProfile struct {
	Preset     string               `json:"preset,omitempty"`
	Weights    ScoreWeights         `json:"weights"`
	Dimensions map[string]Dimension `json:"dimensions,omitempty"`
	// Pinned 是人工锁定的数值：模型的提议会被替换成这里的值（设计 §7.3 / §12.4）
	Pinned  map[string]float64 `json:"pinned,omitempty"`
	Updated int64              `json:"updated,omitempty"`
}

func defaultWeights() ScoreWeights {
	return ScoreWeights{Interest: 1, Novelty: 1, Cost: 0, CostScale: 30}
}

// presetProfile 把预设展开成可以查看的具体数值（不做隐藏的档位）。
func presetProfile(name string) MindProfile {
	switch name {
	case PresetCurious:
		return MindProfile{Preset: PresetCurious, Weights: ScoreWeights{Interest: 1, Novelty: 2, Cost: 0.5, CostScale: 30},
			Dimensions: map[string]Dimension{
				"curiosity":    {Enabled: true, Value: 70, Baseline: 65, Min: 0, Max: 100, Drift: 2, Threshold: 85},
				"social_drive": {Enabled: true, Value: 50, Baseline: 50, Min: 0, Max: 100, Drift: 2},
				"focus":        {Enabled: true, Value: 40, Baseline: 45, Min: 0, Max: 100, Drift: 3},
				"energy":       {Enabled: true, Value: 60, Baseline: 55, Min: 0, Max: 100, Drift: 4},
			}}
	case PresetFocused:
		return MindProfile{Preset: PresetFocused, Weights: ScoreWeights{Interest: 1, Novelty: 0.5, Cost: 1.5, CostScale: 30},
			Dimensions: map[string]Dimension{
				"curiosity":    {Enabled: true, Value: 40, Baseline: 45, Min: 0, Max: 100, Drift: 3},
				"social_drive": {Enabled: true, Value: 30, Baseline: 30, Min: 0, Max: 100, Drift: 2},
				"focus":        {Enabled: true, Value: 80, Baseline: 75, Min: 0, Max: 100, Drift: 2, Threshold: 90},
				"energy":       {Enabled: true, Value: 55, Baseline: 55, Min: 0, Max: 100, Drift: 4},
			}}
	default:
		return MindProfile{Preset: PresetBalanced, Weights: defaultWeights()}
	}
}

// Profile 是当前档案：没设置过就是 balanced，读旧配置不会悄悄改行为。
func (s *State) Profile() MindProfile {
	p := s.MindProfile
	if p.Preset == "" {
		p = presetProfile(PresetBalanced)
	}
	if p.Weights.CostScale <= 0 {
		p.Weights.CostScale = defaultWeights().CostScale
	}
	if p.Dimensions == nil {
		p.Dimensions = map[string]Dimension{}
	}
	return p
}

// SetPreset 展开一个预设或恢复默认（设计 §7.2 的"跟随默认 / 选择预设"）。
func (s *State) SetPreset(name string, now int64) error {
	switch name {
	case PresetBalanced, PresetCurious, PresetFocused, "":
		s.MindProfile = presetProfile(PresetBalanced)
		if name == PresetCurious || name == PresetFocused {
			s.MindProfile = presetProfile(name)
		}
		s.MindProfile.Updated = now
		s.Touch()
		return nil
	}
	return fmt.Errorf("没有这个预设：%s（可以选 balanced / curious / focused 或恢复默认）", name)
}

// SetWeight 改一个选择分权重；范围在这里统一校验，命令、网页和工具共用。
func (s *State) SetWeight(field string, value float64, now int64) error {
	p := s.Profile()
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return errors.New("权重必须是有限数值")
	}
	switch field {
	case "interest", "novelty", "cost":
		if value < 0 || value > 5 {
			return errors.New(field + " 权重只能是 0–5")
		}
		switch field {
		case "interest":
			p.Weights.Interest = value
		case "novelty":
			p.Weights.Novelty = value
		default:
			p.Weights.Cost = value
		}
	case "cost_scale":
		if value != math.Trunc(value) || value < 1 || value > 480 {
			return errors.New("cost_scale 只能是 1–480 的整数分钟")
		}
		p.Weights.CostScale = int(value)
	default:
		return errors.New("可以改的权重：interest、novelty、cost、cost_scale")
	}
	p.Preset = PresetCustom
	p.Updated = now
	s.MindProfile = p
	s.Touch()
	return nil
}

// SetDimension 打开/关闭一个维度，或改它的当前值、基线、范围、漂移和阈值。
func (s *State) SetDimension(name string, d Dimension, now int64) error {
	if !containsString(DimensionNames, name) {
		return fmt.Errorf("没有这个维度：%s（首版内置 curiosity / social_drive / focus / energy）", name)
	}
	if d.Min < 0 || d.Max > 100 || d.Min >= d.Max || d.Value < d.Min || d.Value > d.Max ||
		d.Baseline < d.Min || d.Baseline > d.Max || d.Drift < 0 || d.Drift > 10 || d.Threshold < 0 || d.Threshold > 100 {
		return errors.New("维度的范围、基线、漂移或阈值不合法（值 0–100，漂移 0–10）")
	}
	p := s.Profile()
	if p.Dimensions == nil {
		p.Dimensions = map[string]Dimension{}
	}
	p.Dimensions[name] = d
	p.Preset = PresetCustom
	p.Updated = now
	s.MindProfile = p
	s.Touch()
	return nil
}

// ScoreIdea 是规范化选择分：兴趣和新颖按 1–5 归一化，成本按参照分钟数截顶。
// 默认权重（1/1/0）下和原来的"兴趣+新颖"排序完全一致。
func (p MindProfile) ScoreIdea(x Idea) float64 {
	interest := float64(x.Interest)
	novelty := float64(x.Novelty)
	if interest <= 0 {
		interest = 3
	}
	if novelty <= 0 {
		novelty = 3
	}
	scale := p.Weights.CostScale
	if scale <= 0 {
		scale = 30
	}
	cost := float64(x.CostMinutes)
	if cost < 0 {
		cost = 0
	}
	if cost > float64(scale) {
		cost = float64(scale)
	}
	return p.Weights.Interest*(interest-1)/4 + p.Weights.Novelty*(novelty-1)/4 - p.Weights.Cost*cost/float64(scale)
}

// DriftDimensions 让已启用的维度按各自的漂移速度回落向基线（每次活动结算一次）。
func (s *State) DriftDimensions(now int64) []DimensionChange {
	p := s.Profile()
	if len(p.Dimensions) == 0 {
		return nil
	}
	var changed []DimensionChange
	for _, name := range DimensionNames {
		d, ok := p.Dimensions[name]
		if !ok || !d.Enabled || d.Drift <= 0 {
			continue
		}
		next := d.Value + (d.Baseline-d.Value)*d.Drift/100
		next = math.Round(next*10) / 10
		if next < d.Min {
			next = d.Min
		}
		if next > d.Max {
			next = d.Max
		}
		if next == d.Value {
			continue
		}
		before, armedBefore := d.Value, d.IsArmed()
		d.Value = next
		d.afterCrossing(before)
		p.Dimensions[name] = d
		changed = append(changed, DimensionChange{Name: name, From: before, To: next, Threshold: d.Threshold, Armed: armedBefore})
	}
	if len(changed) > 0 {
		p.Updated = now
		s.MindProfile = p
		s.Touch()
	}
	return changed
}

// DimensionChange 是一次维度变化，Threshold 是它自己的阈值（用于判断要不要发事件）。
type DimensionChange struct {
	Name      string
	From      float64
	To        float64
	Threshold float64
	Armed     bool
}

// Crossed 表示这次变化是否越过了阈值（只报向上穿过，避免回落时也刷事件）。
func (c DimensionChange) Crossed() bool {
	return c.Armed && c.Threshold > 0 && c.From < c.Threshold && c.To >= c.Threshold
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// PinFields 是允许人工锁定的数值：想法和目标的打分输入。
var PinFields = []string{"interest", "novelty", "priority"}

// thresholdHysteresis 是阈值迟滞的回落幅度：掉回阈值以下这么多点才重新装上。
const thresholdHysteresis = 3

// SetPin 锁定一个数值：之后模型自己提的同类数值会被替换成它。
func (s *State) SetPin(field string, value float64, now int64) error {
	if !containsString(PinFields, field) {
		return errors.New("可以锁定的字段：interest、novelty、priority")
	}
	low, high := 1.0, 5.0
	if field == "priority" {
		low, high = 0, 10
	}
	if math.IsNaN(value) || value < low || value > high || value != math.Trunc(value) {
		return fmt.Errorf("%s 只能是 %g–%g 的整数", field, low, high)
	}
	p := s.Profile()
	if p.Pinned == nil {
		p.Pinned = map[string]float64{}
	}
	p.Pinned[field] = value
	p.Updated = now
	s.MindProfile = p
	s.Touch()
	return nil
}

// ClearPin 解除锁定，模型自己提的数值重新生效。
func (s *State) ClearPin(field string, now int64) bool {
	p := s.Profile()
	if _, ok := p.Pinned[field]; !ok {
		return false
	}
	delete(p.Pinned, field)
	p.Updated = now
	s.MindProfile = p
	s.Touch()
	return true
}

// Pin 返回某个字段的人工锁定值；没有锁定返回 ok=false。
func (p MindProfile) Pin(field string) (float64, bool) {
	v, ok := p.Pinned[field]
	return v, ok
}

// Armed 报告这个维度现在能不能报阈值（默认装上）。
func (d Dimension) IsArmed() bool { return d.Armed == nil || *d.Armed }

// afterCrossing 更新迟滞状态：真越过阈值才卸下；回落到阈值以下 hysteresis 才重新装上，
// 在阈值附近来回但没真越过的时候保持原来的状态，不会提前把迟滞拆掉。
func (d *Dimension) afterCrossing(from float64) {
	switch {
	case d.Threshold > 0 && from < d.Threshold && d.Value >= d.Threshold:
		armed := false
		d.Armed = &armed
	case d.Threshold > 0 && d.Value <= d.Threshold-thresholdHysteresis:
		armed := true
		d.Armed = &armed
	}
}
