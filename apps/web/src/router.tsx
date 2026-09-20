import {
  createBrowserHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
} from '@tanstack/react-router';

import { HomePage } from './pages/home-page';
import { LoginPage } from './pages/login-page';

const rootRoute = createRootRoute({
  component: () => <Outlet />,
});

const homeRoute = createRoute({
  component: HomePage,
  getParentRoute: () => rootRoute,
  path: '/',
});

// 登录页独立成路由：未登录时首页会跳到这里，登录成功后回到首页。
const loginRoute = createRoute({
  component: LoginPage,
  getParentRoute: () => rootRoute,
  path: '/login',
});

const routeTree = rootRoute.addChildren([homeRoute, loginRoute]);

// 每次挂载创建独立实例：路由自带可变的位置状态，做成模块级单例会让
// 一次会话的跳转残留到下一位使用者（含测试用例之间）。
export function createAppRouter() {
  return createRouter({
    defaultPreload: 'intent',
    history: createBrowserHistory(),
    routeTree,
  });
}

declare module '@tanstack/react-router' {
  interface Register {
    router: ReturnType<typeof createAppRouter>;
  }
}
