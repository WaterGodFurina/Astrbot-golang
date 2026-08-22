import axios, {
  type AxiosError,
  type AxiosInstance,
  type InternalAxiosRequestConfig,
} from 'axios';
import { getToken, removeToken } from '@/utils/token';

const AUTH_HEADER = 'Authorization';
const LOCALE_HEADER = 'Accept-Language';

let configured = false;
let originalFetch: typeof window.fetch | null = null;

export const httpClient = axios;
export const apiV1Client = axios.create({ baseURL: '/api/v1' });

function getLocale(): string | null {
  return localStorage.getItem('astrbot-locale');
}

function setAxiosHeader(
  headers: InternalAxiosRequestConfig['headers'],
  key: string,
  value: string,
) {
  if (typeof headers.set === 'function') {
    headers.set(key, value);
    return;
  }
  headers[key] = value;
}

// 仅对同源（或相对路径）请求附加 Authorization，避免把 Bearer Token
// 泄露给第三方域名（如插件市场可控的 readme_url）。
function isSameOriginUrl(url: string): boolean {
  return (
    !/^([a-z][a-z\d+\-.]*:)?\/\//i.test(url) ||
    new URL(url, window.location.origin).origin === window.location.origin
  );
}

function attachAxiosHeaders(config: InternalAxiosRequestConfig) {
  const sameOrigin = isSameOriginUrl(config.url || '');

  const token = sameOrigin ? getToken() : null;
  if (token) {
    setAxiosHeader(config.headers, AUTH_HEADER, `Bearer ${token}`);
  }

  const locale = sameOrigin ? getLocale() : null;
  if (locale) {
    setAxiosHeader(config.headers, LOCALE_HEADER, locale);
  }

  return config;
}

function normalizeAxiosError(error: AxiosError) {
  if (error.response?.status === 401) {
    let requestPath = '';
    try {
      const url = error.config?.url || '';
      const baseURL = error.config?.baseURL;
      const resolvedUrl =
        url && baseURL && !/^([a-z][a-z\d+\-.]*:)?\/\//i.test(url)
          ? `${baseURL.replace(/\/+$/, '')}/${url.replace(/^\/+/, '')}`
          : url;
      const requestUrl = new URL(resolvedUrl || '/', window.location.origin);
      if (requestUrl.origin === window.location.origin) {
        requestPath = requestUrl.pathname;
      }
    } catch {
      requestPath = '';
    }

    const isAuthChallenge =
      [
        '/api/auth/login',
        '/api/auth/setup',
        '/api/auth/setup-status',
        '/api/v1/auth/login',
        '/api/v1/auth/setup',
        '/api/v1/auth/setup-status',
      ].includes(requestPath) ||
      Boolean(
        (
          error.response.data as
            | { data?: { totp_required?: boolean } }
            | undefined
        )?.data?.totp_required,
      );

    if (requestPath.startsWith('/api/') && !isAuthChallenge) {
      removeToken();
      ['user', 'change_pwd_hint', 'md5_pwd_hint', 'password_upgrade_required'].forEach(
        (key) => localStorage.removeItem(key),
      );

      if (!window.location.hash.startsWith('#/auth/login')) {
        window.location.hash = '/auth/login';
      }
    }
  }

  if (error.response?.status === 429) {
    const data = error.response.data as { message?: string } | undefined;
    if (data?.message) {
      return Promise.reject(data.message);
    }
  }
  return Promise.reject(error);
}

function installAxiosInterceptors(instance: AxiosInstance) {
  instance.interceptors.request.use(attachAxiosHeaders);
  instance.interceptors.response.use((response) => response, normalizeAxiosError);
}

function isSameOriginRequest(input: RequestInfo | URL): boolean {
  try {
    const url =
      typeof input === 'string'
        ? new URL(input, window.location.origin)
        : input instanceof URL
          ? input
          : new URL(input.url, window.location.origin);
    return url.origin === window.location.origin;
  } catch {
    return false;
  }
}

export function fetchWithAuth(input: RequestInfo | URL, init?: RequestInit) {
  const fetchImpl = originalFetch ?? window.fetch.bind(window);
  const token = getToken();
  const locale = getLocale();

  // 只对同源请求附加凭证，第三方域名/非 HTTP 目标一律原样转发。
  if ((!token && !locale) || !isSameOriginRequest(input)) {
    return fetchImpl(input, init);
  }

  const requestHeaders =
    typeof input !== 'string' && 'headers' in input
      ? (input as Request).headers
      : undefined;
  const headers = new Headers(init?.headers || requestHeaders);

  if (token && !headers.has(AUTH_HEADER)) {
    headers.set(AUTH_HEADER, `Bearer ${token}`);
  }
  if (locale && !headers.has(LOCALE_HEADER)) {
    headers.set(LOCALE_HEADER, locale);
  }

  return fetchImpl(input, { ...init, headers });
}

export function setupHttpClient() {
  if (configured) {
    return;
  }

  installAxiosInterceptors(axios);
  installAxiosInterceptors(apiV1Client);

  originalFetch = window.fetch.bind(window);
  window.fetch = fetchWithAuth;

  configured = true;
}
