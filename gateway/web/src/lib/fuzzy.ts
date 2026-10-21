// 命令面板用的模糊匹配：连续出现的子串得分最高，越靠前越好；其次是按顺序出现的字符。

export function score(query: string, text: string): number {
  const q = query.trim().toLowerCase();
  if (!q) return 1;
  const t = text.toLowerCase();
  const idx = t.indexOf(q);
  if (idx >= 0) return 1000 - idx * 2 - (t.length - q.length) * 0.1;
  // 每个词都要出现
  const words = q.split(/\s+/).filter(Boolean);
  if (words.length > 1) {
    let total = 0;
    for (const w of words) {
      const s = score(w, text);
      if (s < 0) return -1;
      total += s;
    }
    return total / words.length - 50;
  }
  // 按顺序出现的字符
  let ti = 0;
  let gaps = 0;
  for (const ch of q) {
    const j = t.indexOf(ch, ti);
    if (j < 0) return -1;
    gaps += j - ti;
    ti = j + 1;
  }
  return 300 - gaps * 3;
}
