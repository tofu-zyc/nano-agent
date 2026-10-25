// 定时任务的运行时间：界面上用几种常见写法来选，存成网关认的字符串
// （cron 五段式、every 30m、at YYYY-MM-DD HH:mm，见 internal/schedule）。
// 认不出来的写法（比如每月 1 号的 cron）落到「自定义」，原样保留。

export type WhenKind = 'every' | 'daily' | 'weekly' | 'once' | 'custom';

export type WhenForm = {
  kind: WhenKind;
  n: number; // 每隔几
  unit: 'm' | 'h'; // 分钟 / 小时
  time: string; // 每天、每周的几点：HH:MM
  days: number[]; // 每周哪几天：0 是周日，1–6 是周一到周六
  at: string; // 只运行一次的时间：YYYY-MM-DDTHH:MM（datetime-local 的格式）
  custom: string; // 自定义的写法
};

/** 每周的几天按周一到周日排 */
export const weekdays: { d: number; label: string }[] = [
  { d: 1, label: '一' },
  { d: 2, label: '二' },
  { d: 3, label: '三' },
  { d: 4, label: '四' },
  { d: 5, label: '五' },
  { d: 6, label: '六' },
  { d: 0, label: '日' },
];

const pad = (n: number) => String(n).padStart(2, '0');
const byWeek = (a: number, b: number) => ((a + 6) % 7) - ((b + 6) % 7);

/** 只运行一次的默认时间：一小时后的整点 */
export function defaultAt(): string {
  const d = new Date(Date.now() + 3600_000);
  d.setMinutes(0, 0, 0);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:00`;
}

// cron 的星期段：1,3,5、1-5、0-6 这类；别的写法返回 null
function cronDays(s: string): number[] | null {
  const out = new Set<number>();
  for (const part of s.split(',')) {
    const r = /^([0-7])(?:-([0-7]))?$/.exec(part);
    if (!r) return null;
    const a = +r[1];
    const b = r[2] === undefined ? a : +r[2];
    if (b < a) return null;
    for (let d = a; d <= b; d++) out.add(d % 7);
  }
  return [...out].sort(byWeek);
}

export function parseWhen(when: string): WhenForm {
  const w = when.trim().replace(/\s+/g, ' ');
  const f: WhenForm = { kind: 'custom', n: 30, unit: 'm', time: '08:00', days: [1, 2, 3, 4, 5], at: defaultAt(), custom: w };
  let m = /^every (\d+) ?(m|h)$/.exec(w);
  if (m && +m[1] > 0) return { ...f, kind: 'every', n: +m[1], unit: m[2] as 'm' | 'h' };
  m = /^at (\d{4}-\d{2}-\d{2}) (\d{2}:\d{2})$/.exec(w);
  if (m) return { ...f, kind: 'once', at: m[1] + 'T' + m[2] };
  m = /^(\d{1,2}) (\d{1,2}) \* \* (\S+)$/.exec(w);
  if (m && +m[1] < 60 && +m[2] < 24) {
    const time = pad(+m[2]) + ':' + pad(+m[1]);
    if (m[3] === '*') return { ...f, kind: 'daily', time };
    const days = cronDays(m[3]);
    if (days) return { ...f, kind: days.length === 7 ? 'daily' : 'weekly', time, days: days.length === 7 ? f.days : days };
  }
  return f;
}

/** 表单 → 网关认的写法；填得不完整时返回错误说明 */
export function buildWhen(f: WhenForm): { when: string; error?: string } {
  const [h, mi] = (f.time || '').split(':').map(Number);
  const clock = Number.isInteger(h) && Number.isInteger(mi);
  switch (f.kind) {
    case 'every':
      if (!(f.n >= 1)) return { when: '', error: '间隔至少是 1' };
      return { when: `every ${Math.floor(f.n)}${f.unit}` };
    case 'daily':
      if (!clock) return { when: '', error: '选一个时间' };
      return { when: `${mi} ${h} * * *` };
    case 'weekly':
      if (!clock) return { when: '', error: '选一个时间' };
      if (!f.days.length) return { when: '', error: '至少选一天' };
      return { when: `${mi} ${h} * * ${[...f.days].sort(byWeek).join(',')}` };
    case 'once':
      if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}/.test(f.at)) return { when: '', error: '选一个日期和时间' };
      return { when: 'at ' + f.at.slice(0, 16).replace('T', ' ') };
    default:
      if (!f.custom.trim()) return { when: '', error: '写一个运行时间' };
      return { when: f.custom.trim() };
  }
}

/** 给人看的说明 */
export function describeWhen(f: WhenForm): string {
  switch (f.kind) {
    case 'every':
      return `每 ${f.n} ${f.unit === 'm' ? '分钟' : '小时'}运行一次`;
    case 'daily':
      return `每天 ${f.time} 运行`;
    case 'weekly': {
      const names = weekdays.filter((w) => f.days.includes(w.d)).map((w) => '周' + w.label);
      return names.length ? `每${names.join('、')} ${f.time} 运行` : '还没选哪几天';
    }
    case 'once':
      return `${f.at.replace('T', ' ')} 运行一次，跑完自动关闭`;
    default:
      return '按自定义写法运行';
  }
}
