<!-- SPDX-License-Identifier: Apache-2.0 -->
<script lang="ts">
  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';
  onMount(() => { void goto('/providers', { replaceState: true }); });
</script>
<p>Provider settings have moved to <a href="/providers">Providers</a>.</p>
