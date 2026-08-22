import { createRouter, createWebHashHistory } from 'vue-router';
import MainRoutes from './MainRoutes';
import AuthRoutes from './AuthRoutes';
import ChatBoxRoutes from './ChatBoxRoutes';
import { useAuthStore } from '@/stores/auth';
import { useRouterLoadingStore } from '@/stores/routerLoading';

export const router = createRouter({
  history: createWebHashHistory(import.meta.env.BASE_URL),
  routes: [
    MainRoutes,
    AuthRoutes,
    ChatBoxRoutes
  ]
});

interface AuthStore {
  username: string;
  returnUrl: string | null;
  login(
    username: string,
    password: string,
    code?: string,
    trustDeviceToken?: boolean,
  ): Promise<void | 'totp_required' | 'upgrade_recovery_required'>;
  logout(): void;
  has_token(): boolean;
}

router.beforeEach(async (to, from, next) => {
  if (from.name && from.path !== to.path) {
    const loadingStore = useRouterLoadingStore();
    loadingStore.start();
  }

  const auth: AuthStore = useAuthStore();

  // 如果用户已登录且试图访问登录页面，则重定向到首页
  if (to.path === '/auth/login' && auth.has_token()) {
    return next('/welcome');
  }

  // 统一以 meta.requiresAuth 为唯一判断来源（未显式关闭鉴权的路由默认
  // 要求登录），避免漏配 requiresAuth 的路由被放行。前端守卫只是 UX 层，
  // token 有效性由后端 401 拦截兜底。
  const requiresAuth = to.matched.every(
    (record) => record.meta.requiresAuth !== false,
  );
  if (requiresAuth && !auth.has_token()) {
    auth.returnUrl = to.fullPath;
    return next('/auth/login');
  }
  return next();
});

router.afterEach(() => {
  const loadingStore = useRouterLoadingStore();
  loadingStore.finish();
});
