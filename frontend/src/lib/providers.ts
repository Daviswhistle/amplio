// SPDX-License-Identifier: Apache-2.0
export type ProviderID = 'chatgpt' | 'openai';
export type Choice = { provider: ProviderID; model: string; effort: string };
export type ModelOption = { id: string; name: string };
export type ProviderStatus = {
  api_key_source: 'none' | 'stored' | 'environment';
  chatgpt_signed_in: boolean;
  chatgpt_connected: boolean;
  chatgpt_email?: string;
  login_state: string;
  login_error?: string;
  login_pending: boolean;
  login_expires_at?: string;
  setup_required: boolean;
  settings: { agent: string; hq: string; fast: string };
};
async function request<T>(path: string, method = 'GET', body?: unknown): Promise<T> {
  const response = await fetch(`/api/providers${path}`, {
    method, credentials: 'same-origin', cache: 'no-store',
    headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body)
  });
  const result = await response.json();
  if (!response.ok) throw new Error(result.error || `Provider request failed (${response.status})`);
  return result as T;
}
export const providers = {
  status: () => request<ProviderStatus>(''),
  saveKey: (key: string) => request<ProviderStatus>('/api-key', 'POST', { key }),
  deleteKey: () => request<ProviderStatus>('/api-key', 'DELETE'),
  login: (newAccount = false) => request<{ authorization_url: string; redirect_uri: string; expires_at: string }>('/chatgpt/login', 'POST', { new_account: newAccount }),
  cancelLogin: () => request<ProviderStatus>('/chatgpt/login', 'DELETE'),
  completeLogin: (url: string) => request<ProviderStatus>('/chatgpt/callback', 'POST', { url }),
  disconnect: () => request<ProviderStatus>('/chatgpt', 'DELETE'),
  models: (provider: ProviderID) => request<{ models: ModelOption[] }>(`/models?provider=${provider}`),
  addModel: (choice: Choice) => request<{ spec: string }>('/models', 'POST', choice),
  saveSettings: (settings: { agent: Choice; hq: Choice; fast: Choice }) => request<ProviderStatus>('/settings', 'PUT', settings)
};
export function modelSpec(choice: Choice): string {
  const prefix = choice.provider === 'chatgpt' ? 'chatgpt' : 'openai-responses';
  return `${prefix}:${choice.model.trim()}${choice.effort ? `?reasoning.effort=${encodeURIComponent(choice.effort)}` : ''}`;
}
export function parseChoice(spec: string): Choice {
  const match = spec.match(/^(chatgpt|openai-responses):([^?#]+)(?:\?([^#]*))?/);
  return match ? { provider: match[1] === 'chatgpt' ? 'chatgpt' : 'openai', model: match[2], effort: new URLSearchParams(match[3] || '').get('reasoning.effort') || '' } : { provider: 'chatgpt', model: '', effort: '' };
}
