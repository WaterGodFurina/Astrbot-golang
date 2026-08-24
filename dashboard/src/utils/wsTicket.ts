import { apiV1Client } from '@/api/http';

// 换取 WebSocket/下载一次性票据（POST /api/v1/auth/ws-ticket）。
// 仅当端点不存在（404，旧后端）时返回空串；网络抖动、超时、5xx 等
// 一律抛出异常——调用方不得回退到把长效 token 放进 URL。
export async function getWSTicket(): Promise<string> {
  try {
    const response = await apiV1Client.post('/auth/ws-ticket', undefined, {
      timeout: 5000,
    });
    const ticket = response.data?.data?.ticket ?? response.data?.ticket;
    return typeof ticket === 'string' && ticket ? ticket : '';
  } catch (error) {
    const status = (error as { response?: { status?: number } })?.response
      ?.status;
    if (status === 404) {
      return '';
    }
    console.warn('ws-ticket fetch failed, refusing token-in-url fallback:', error);
    throw error;
  }
}