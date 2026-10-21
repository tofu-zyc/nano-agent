// 浏览内核那台机器上的目录（网关转给内核列出，只有子目录名）
import { api } from './api';

export type DirEntry = { name: string; blocked?: boolean };
export type DirListing = { path: string; home: string; blocked: boolean; entries: DirEntry[]; truncated: boolean };

export function listDir(path: string): Promise<DirListing> {
  return api.get<DirListing>('/api/fs?path=' + encodeURIComponent(path));
}

/** 主目录下的路径写成 ~ 开头，短一些，换了机器也能用 */
export function toTilde(path: string, home: string): string {
  if (!home || home === '/') return path;
  if (path === home) return '~';
  if (path.startsWith(home + '/')) return '~' + path.slice(home.length);
  return path;
}

export function join(dir: string, name: string): string {
  if (dir === '/') return '/' + name;
  return dir.replace(/\/+$/, '') + '/' + name;
}

/** 把正在输入的路径拆成“已经确定的上级目录”和“正在输入的最后一段” */
export function splitTyping(text: string): { parent: string; prefix: string } | null {
  if (!text) return { parent: '~', prefix: '' };
  if (text === '~') return { parent: '~', prefix: '' };
  if (!text.startsWith('/') && !text.startsWith('~/')) return null;
  const i = text.lastIndexOf('/');
  const parent = i === 0 ? '/' : text.slice(0, i);
  return { parent: parent || '/', prefix: text.slice(i + 1) };
}
