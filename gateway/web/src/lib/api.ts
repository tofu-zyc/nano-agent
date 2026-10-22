// 网关接口。改动类请求都带 X-Nano 头（网关据此防跨站请求）。
import type { Attachment } from './types';

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message);
  }
}

let onUnauthorized: () => void = () => {};
export function setUnauthorizedHandler(f: () => void) {
  onUnauthorized = f;
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: { 'X-Nano': '1', ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}) },
    body: body !== undefined ? JSON.stringify(body) : undefined,
    credentials: 'same-origin',
  });
  let data: any = null;
  try {
    data = await res.json();
  } catch {
    /* 没有 JSON 正文 */
  }
  if (!res.ok) {
    if (res.status === 401 && path !== '/api/login') onUnauthorized();
    throw new ApiError(res.status, data?.error || `${res.status} ${res.statusText}`);
  }
  return data as T;
}

export const api = {
  get: <T>(path: string) => call<T>('GET', path),
  post: <T>(path: string, body: unknown = {}) => call<T>('POST', path, body),
  patch: <T>(path: string, body: unknown) => call<T>('PATCH', path, body),
  put: <T>(path: string, body: unknown) => call<T>('PUT', path, body),
  del: <T>(path: string) => call<T>('DELETE', path),
};

export const sidPath = (sid: string) => `/api/sessions/${encodeURIComponent(sid)}`;

/** 上传一张图片，返回它的引用（发消息时带上 id） */
export async function uploadImage(file: File): Promise<Attachment> {
  const fd = new FormData();
  fd.append('file', file);
  const res = await fetch('/api/uploads', { method: 'POST', headers: { 'X-Nano': '1' }, body: fd, credentials: 'same-origin' });
  let data: any = null;
  try {
    data = await res.json();
  } catch {
    /* 没有 JSON */
  }
  if (!res.ok) {
    if (res.status === 401) onUnauthorized();
    throw new ApiError(res.status, data?.error || `${res.status} ${res.statusText}`);
  }
  return data as Attachment;
}
