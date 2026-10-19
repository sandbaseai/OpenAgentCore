import { chromium } from 'playwright';
import { writeFileSync } from 'node:fs';
// Harnesses consume explicit tool variables, not the daemon's ambient environment.
const browser = chromium.executablePath();
writeFileSync('/opt/oac-creative/tool-env.json', JSON.stringify({
  PLAYWRIGHT_BROWSERS_PATH: '/opt/oac-creative/browsers',
  HYPERFRAMES_BROWSER_PATH: browser,
  PUPPETEER_EXECUTABLE_PATH: browser,
}) + '\n');
