// 一个很小、只输出自己生成的标签的 Markdown 渲染器：先整体转义 HTML，再识别代码块、列表、
// 标题、引用、行内代码、加粗、链接。模型的回复不会带来任何可执行的 HTML。

function esc(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

function inline(s: string): string {
  // 行内代码先拿出来，里面的内容不再处理
  const codes: string[] = [];
  s = s.replace(/`([^`\n]+)`/g, (_, c) => {
    codes.push(`<code>${c}</code>`);
    return `\u0000${codes.length - 1}\u0000`;
  });
  s = s.replace(/\*\*([^*\n]+)\*\*/g, '<strong>$1</strong>');
  s = s.replace(/(^|[^*])\*([^*\n]+)\*(?!\*)/g, '$1<em>$2</em>');
  s = s.replace(/\[([^\]\n]+)\]\((https?:\/\/[^)\s]+)\)/g, '<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>');
  s = s.replace(/(^|[\s(])(https?:\/\/[^\s<)]+)/g, '$1<a href="$2" target="_blank" rel="noopener noreferrer">$2</a>');
  return s.replace(/\u0000(\d+)\u0000/g, (_, i) => codes[Number(i)]);
}

export function markdown(src: string): string {
  const lines = esc(src.replace(/\r\n/g, '\n')).split('\n');
  const out: string[] = [];
  let i = 0;
  let para: string[] = [];
  const flush = () => {
    if (para.length) out.push(`<p>${para.map(inline).join('<br>')}</p>`);
    para = [];
  };
  while (i < lines.length) {
    const line = lines[i];
    const fence = line.match(/^\s*```\s*([\w+-]*)\s*$/);
    if (fence) {
      flush();
      const body: string[] = [];
      i++;
      while (i < lines.length && !/^\s*```\s*$/.test(lines[i])) body.push(lines[i++]);
      i++;
      const lang = fence[1] ? `<span class="lang">${fence[1]}</span>` : '';
      out.push(`<pre>${lang}<code>${body.join('\n')}</code></pre>`);
      continue;
    }
    const h = line.match(/^(#{1,6})\s+(.*)$/);
    if (h) {
      flush();
      out.push(`<h4>${inline(h[2])}</h4>`);
      i++;
      continue;
    }
    if (/^\s*([-*]|\d+[.)])\s+/.test(line)) {
      flush();
      const ordered = /^\s*\d+[.)]\s+/.test(line);
      const items: string[] = [];
      while (i < lines.length && /^\s*([-*]|\d+[.)])\s+/.test(lines[i])) {
        items.push(`<li>${inline(lines[i].replace(/^\s*([-*]|\d+[.)])\s+/, ''))}</li>`);
        i++;
      }
      out.push(ordered ? `<ol>${items.join('')}</ol>` : `<ul>${items.join('')}</ul>`);
      continue;
    }
    if (/^&gt;\s?/.test(line)) {
      flush();
      const q: string[] = [];
      while (i < lines.length && /^&gt;\s?/.test(lines[i])) q.push(inline(lines[i++].replace(/^&gt;\s?/, '')));
      out.push(`<blockquote>${q.join('<br>')}</blockquote>`);
      continue;
    }
    if (!line.trim()) {
      flush();
      i++;
      continue;
    }
    para.push(line);
    i++;
  }
  flush();
  return out.join('');
}
