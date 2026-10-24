package config

import "testing"

// 每个配置项只有一个家：group 是拓扑图上的一个节点，key 对应配置里真实存在的值，不重复；
// 每个节点至少挂着一项（没有设置的节点不该出现在 Nodes 里）。网页按 group 渲染，这里不漏，网页就不漏。
func TestEveryFieldHasAHome(t *testing.T) {
	s, _, err := Open(writeConf(t))
	if err != nil {
		t.Fatal(err)
	}
	cur := asMap(s.Current())
	seen := map[string]bool{}
	count := map[string]int{}
	for _, f := range Fields {
		if seen[f.Key] {
			t.Errorf("%s 重复了", f.Key)
		}
		seen[f.Key] = true
		if _, ok := NodeByID(f.Group); !ok {
			t.Errorf("%s 的 group %q 不是拓扑图上的节点（见 Nodes）", f.Key, f.Group)
		}
		count[f.Group]++
		if _, ok := getPath(cur, f.Key); !ok {
			t.Errorf("%s 在配置里没有对应的值", f.Key)
		}
		if f.Label == "" || f.Type == "" {
			t.Errorf("%s 缺少名字或类型", f.Key)
		}
		if (f.Type == "enum" || f.Type == "multi") && len(f.Options) == 0 {
			t.Errorf("%s 没有可选值", f.Key)
		}
	}
	for _, n := range Nodes {
		if count[n.ID] == 0 {
			t.Errorf("节点 %s 上没有设置", n.ID)
		}
	}
}
