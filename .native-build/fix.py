from pathlib import Path

def replace(path, old, new):
    p=Path(path); text=p.read_text(); assert text.count(old)==1,(path,old[:80]);p.write_text(text.replace(old,new))

replace('.github/workflows/providers.yml','runs-on: ubuntu-latest','runs-on: ubuntu-24.04')
p='frontend/src/routes/providers/+page.svelte'
replace(p,"const newlyConnected = next.chatgpt_connected && !status?.chatgpt_connected;","const completedLogin = next.login_state === 'complete' && status?.login_state !== 'complete';\n    const newlyConnected = next.chatgpt_connected && (!status?.chatgpt_connected || completedLogin);")
replace(p,"'Sign in with ChatGPT'","'Continue with ChatGPT'")
replace('docs/guide/providers.md','**Sign in with ChatGPT**','**Continue with ChatGPT**')
replace('tests/e2e/providers.mjs'," const ended = new Promise(r => child.once('exit', r)); child.kill('SIGINT');\n await Promise.race([ended, sleep(10000).then(() => { if (child.exitCode === null) child.kill('SIGKILL'); })]);", " const process = child;\n const ended = new Promise(r => process.once('exit', r));\n const timer = setTimeout(() => { if (process.exitCode === null) process.kill('SIGKILL'); }, 10000);\n process.kill('SIGINT');\n try { await ended; } finally { clearTimeout(timer); }")
