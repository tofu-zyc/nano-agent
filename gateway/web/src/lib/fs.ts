// 浏览一台机器上的目录（本机和 SSH 服务器由网关列，装了内核的电脑由它的内核列；只有子目录名）
import { api } from './api';

export type DirEntry = { name: string; blocked?: boolean };
export type DirListing = {
  path: string;
  home: string;
  os?: 'linux' | 'macos' | 'windows' | string;
  blocked: boolean;
  entries: DirEntry[];
  truncated: boolean;
};

/** 列出机器 host 上的目录；host 为空时是本机（网关所在的机器） */
export function listDir(path: string, host = ''): Promise<DirListing> {
  return api.get<DirListing>('/api/fs?path=' + encodeURIComponent(path) + (host ? '&host=' + encodeURIComponent(host) : ''));
}

/** Windows 路径（盘符或 UNC）；路径属于它所在的机器，前端只做显示，不解析 */
export function isWindowsPath(p: string): boolean {
  return /^[A-Za-z]:[\\/]/.test(p) || p.startsWith('\\\\');
}

export function isWindowsOS(os?: string): boolean {
  return os === 'windows';
}

/** 主目录下的路径写成 ~ 开头，短一些，换了机器也能用 */
export function toTilde(path: string, home: string): string {
  if (!home) return path;
  if (isWindowsPath(path) && isWindowsPath(home)) {
    if (path.toLowerCase() === home.toLowerCase()) return '~';
    if (path.toLowerCase().startsWith(home.toLowerCase() + '\\')) return '~' + path.slice(home.length);
    return path;
  }
  if (home === '/') return path;
  if (path === home) return '~';
  if (path.startsWith(home + '/')) return '~' + path.slice(home.length);
  return path;
}

export function join(dir: string, name: string): string {
  if (!dir) return name;
  if (isWindowsPath(dir) || isWindowsPath(name)) {
    if (isWindowsPath(name)) return name;
    return dir.replace(/[\\/]+$/, '') + '\\' + name;
  }
  if (dir === '/') return '/' + name;
  return dir.replace(/\/+$/, '') + '/' + name;
}

/** 把正在输入的路径拆成“已经确定的上级目录”和“正在输入的最后一段” */
export function splitTyping(text: string): { parent: string; prefix: string } | null {
  if (!text) return { parent: '~', prefix: '' };
  if (text === '~') return { parent: '~', prefix: '' };
  if (isWindowsPath(text)) {
    const m = text.match(/^(.*[\\/])([^\\/]*)$/);
    if (!m) return { parent: text, prefix: '' };
    let parent = m[1].replace(/[\\/]+$/, '');
    if (/^[A-Za-z]:$/.test(parent)) parent += '\\';
    return { parent: parent || m[1], prefix: m[2] };
  }
  if (!text.startsWith('/') && !text.startsWith('~/')) return null;
  const i = text.lastIndexOf('/');
  const parent = i === 0 ? '/' : text.slice(0, i);
  return { parent: parent || '/', prefix: text.slice(i + 1) };
}
