# Native OpenAI Responses and ChatGPT support

The experimental Python bridge has been replaced by native Go providers and
in-app setup. See [Providers](providers.md) for login, API keys, model selection,
remote-browser completion, migration and the verification boundary.

No Python, pip, Codex CLI or separate sign-in executable is required. Existing
draft OpenAI subprocess specs are recognized by the native provider factory;
other Amplio subprocess bridges continue to work unchanged.
