import type { JobInfo, JobOutput } from './types';

export const jobStates: Record<string, string> = {
  queued: '排队中', cancelling: '正在取消', starting: '正在启动', running: '运行中', preparing: '正在移交',
  succeeded: '已完成', failed: '失败', cancelled: '已取消', timed_out: '已超时',
  interrupted: '重启后中断', unknown: '状态未知',
};
export function finalJob(status: string): boolean {
  return ['succeeded', 'failed', 'cancelled', 'timed_out', 'interrupted', 'unknown'].includes(status);
}
export function jobKey(job: Pick<JobInfo, 'node' | 'job_id'>): string {
  return JSON.stringify([job.node, job.job_id]);
}
export function mergeJob(current: JobInfo | undefined, incoming: JobInfo): JobInfo {
  if (!current || jobKey(current) !== jobKey(incoming)) return incoming;
  if (incoming.revision < current.revision || finalJob(current.status) && !finalJob(incoming.status)) return current;
  // A status HTTP reply and an SSE update may race, including detach/terminal.
  return incoming;
}

/** Bounded view of binary log pages. UTF-8 can straddle chunks and requests. */
export class JobLog {
  text = '';
  cursor = 0;
  gap = false;
  private decoders = new Map<string, TextDecoder>();
  append(output: JobOutput, final = false) {
    if (output.chunks.length > 64 || !Number.isSafeInteger(output.next_cursor) || output.next_cursor < this.cursor) {
      throw new Error('任务日志游标无效');
    }
    const chunks: { stream: string; raw: string }[] = [];
    let bytes = 0, end = this.cursor;
    for (const chunk of output.chunks) {
      if (!Number.isSafeInteger(chunk.cursor) || !Number.isSafeInteger(chunk.next_cursor) ||
          chunk.cursor < end || chunk.next_cursor < chunk.cursor || chunk.next_cursor > output.next_cursor ||
          chunk.data.length > 90000 || !['stdout', 'stderr', 'output'].includes(chunk.stream)) {
        throw new Error('任务日志分片无效');
      }
      const raw = atob(chunk.data);
      bytes += raw.length;
      if (bytes > 65536) throw new Error('任务日志超过读取上限');
      chunks.push({ stream: chunk.stream, raw });
      end = chunk.next_cursor;
    }
    // Validate the whole page before advancing a streaming decoder or cursor.
    if (output.gap || output.truncated_before > this.cursor) {
      this.gap = true;
      this.decoders.clear();
    }
    const decoded: string[] = [];
    for (const { stream, raw } of chunks) {
      let decoder = this.decoders.get(stream);
      if (!decoder) { decoder = new TextDecoder(); this.decoders.set(stream, decoder); }
      decoded.push(decoder.decode(Uint8Array.from(raw, c => c.charCodeAt(0)), { stream: true }));
    }
    if (final && output.next_cursor >= output.output_cursor) {
      for (const decoder of this.decoders.values()) decoded.push(decoder.decode());
      this.decoders.clear();
    }
    this.cursor = output.next_cursor;
    this.text = (this.text + decoded.join('')).slice(-131072);
  }
}
