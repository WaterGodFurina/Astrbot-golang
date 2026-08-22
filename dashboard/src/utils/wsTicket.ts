import { apiV1Client } from '@/api/http';

// 换取 WebSocket 一次性票据（POST /api/v1/auth/ws-ticket）。
// 端点不可用 / 网络异常时返回空串，调用方回退到 ?token= 旧行为。
export async function getWSTicket(): Promise<string> {
  try {
    const response = await apiV1Client.post('/auth/ws-ticket', undefined, {
      timeout: 5000,
    });
    const ticket = response.data?.data?.ticket ?? response.data?.ticket;
    return typeof ticket === 'string' && ticket ? ticket : '';
  } catch {
    return '';
  }
}