import type { ThemeConfig } from 'antd';

export const jrpTheme: ThemeConfig = {
  token: {
    borderRadius: 2,
    colorBgBase: '#07100f',
    colorBgContainer: '#0b1716',
    colorBgElevated: '#10211f',
    colorBorder: '#21413b',
    colorError: '#ff6b5f',
    colorInfo: '#39d9c5',
    colorPrimary: '#39d9c5',
    colorSuccess: '#7ce69d',
    colorText: '#e6f5ef',
    colorTextSecondary: '#89a59c',
    colorWarning: '#f3c96b',
    fontFamily: 'Bahnschrift, "Microsoft YaHei UI", "PingFang SC", sans-serif',
    fontFamilyCode: '"Cascadia Code", "Sarasa Mono SC", ui-monospace, monospace',
    wireframe: false,
  },
  components: {
    Card: {
      colorBgContainer: 'rgba(11, 23, 22, 0.86)',
      headerBg: 'transparent',
    },
    Tag: {
      borderRadiusSM: 2,
    },
  },
};
