import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider } from '@tanstack/react-router';
import { useState } from 'react';

import { JrpThemeProvider } from '@jrp/ui';

import { createAppRouter } from './router';

function createQueryClient() {
  return new QueryClient({
    defaultOptions: {
      queries: {
        refetchOnWindowFocus: false,
        retry: false,
      },
    },
  });
}

export function App() {
  const [queryClient] = useState(createQueryClient);
  const [appRouter] = useState(createAppRouter);

  return (
    <JrpThemeProvider>
      <QueryClientProvider client={queryClient}>
        <RouterProvider router={appRouter} />
      </QueryClientProvider>
    </JrpThemeProvider>
  );
}
