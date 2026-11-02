from pathlib import Path
import json
import subprocess

def replace(path, old, new):
    p=Path(path); text=p.read_text(); assert text.count(old)==1,(path,old[:80]);p.write_text(text.replace(old,new))

replace('.github/workflows/providers.yml','runs-on: ubuntu-latest','runs-on: ubuntu-24.04')
replace('.github/workflows/providers.yml','npm ci && npm run check','npm ci && npm run check && npm audit --audit-level=high')
replace('.github/workflows/providers.yml','          path: amplio\n','          path: |\n            amplio\n            LICENSE\n            THIRD_PARTY_NOTICES.txt\n')
p='frontend/src/routes/providers/+page.svelte'
replace(p,"const newlyConnected = next.chatgpt_connected && !status?.chatgpt_connected;","const completedLogin = next.login_state === 'complete' && status?.login_state !== 'complete';\n    const newlyConnected = next.chatgpt_connected && (!status?.chatgpt_connected || completedLogin);")
replace(p,"'Sign in with ChatGPT'","'Continue with ChatGPT'")
replace('docs/guide/providers.md','**Sign in with ChatGPT**','**Continue with ChatGPT**')
replace('tests/e2e/providers.mjs'," const ended = new Promise(r => child.once('exit', r)); child.kill('SIGINT');\n await Promise.race([ended, sleep(10000).then(() => { if (child.exitCode === null) child.kill('SIGKILL'); })]);", " const process = child;\n const ended = new Promise(r => process.once('exit', r));\n const timer = setTimeout(() => { if (process.exitCode === null) process.kill('SIGKILL'); }, 10000);\n process.kill('SIGINT');\n try { await ended; } finally { clearTimeout(timer); }")

# Match the upstream-maintained security update versions, rather than forcing a
# SvelteKit major upgrade for unrelated low-severity development dependencies.
replace('frontend/package.json','"dompurify": "^3.4.16"','"dompurify": "^3.4.17"')
replace('frontend/package.json','"vite": "^8.0.7"','"vite": "^8.3.5"')
subprocess.run(['npm','install','--ignore-scripts','--no-fund','--no-audit'],cwd='frontend',check=True)
subprocess.run(['npm','audit','--audit-level=high'],cwd='frontend',check=True)

# Refresh only the npm attribution section using the repository's own generator;
# Go modules/model weights are unchanged and their notices are retained verbatim.
script=Path('scripts/gen-third-party-notices.sh').read_text()
node=script.split('<<\'NODE\'\n',1)[1].split('\nNODE\n',1)[0]
new_notices=subprocess.check_output(['node','-',str(Path('frontend').resolve())],input=node,text=True)
notice=Path('THIRD_PARTY_NOTICES.txt')
old=notice.read_text()
marker='NPM PACKAGES (bundled into the amplio web UI)\n'+'='*80+'\n\n'
assert old.count(marker)==1
notice.write_text(old.split(marker,1)[0]+marker+new_notices)
