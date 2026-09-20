import { describe, expect, it } from 'vitest';

import { createTargetListHandler, http, HttpResponse, mockNotificationTarget } from '@jrp/devmock';
import { mockServer } from '@jrp/devmock/server';

import { fetchTargets } from './notification';

describe('通知目标 API 客户端', () => {
  it('解析服务端的 items 包裹结构', async () => {
    mockServer.use(createTargetListHandler([mockNotificationTarget]));

    await expect(fetchTargets()).resolves.toEqual([mockNotificationTarget]);
  });

  it('拒绝裸数组响应，避免与真实契约漂移', async () => {
    // jrps 的列表响应固定为 {items:[...]}；裸数组若被接受，说明前端契约已经和
    // 服务端脱节，宁可显式报错也不要装作读到了列表。
    mockServer.use(
      http.get('/api/v1/notification-targets', () => HttpResponse.json([mockNotificationTarget])),
    );

    await expect(fetchTargets()).rejects.toThrow('通知目标列表返回了无法识别的数据');
  });
});
