#!/usr/bin/env node
import { mkdir, readFile } from 'node:fs/promises';
import { spawnSync } from 'node:child_process';
import { resolve } from 'node:path';

const toolEnv = JSON.parse(await readFile('/opt/oac-creative/tool-env.json', 'utf8'));
Object.assign(process.env, toolEnv);
const { chromium } = await import('playwright');
const output = process.argv[2];
if (output && !output.startsWith('/')) throw new Error('Use an absolute output directory');
const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
try {
  const page = await browser.newPage({ viewport: { width: 640, height: 360 } });
  await page.setContent('<html lang="zh-CN"><meta charset="utf-8"><style>body{font-family:"Noto Sans CJK SC",sans-serif;background:#172033;color:white}h1{font-size:40px}</style><h1>制作环境 Ready</h1><canvas width="100" height="100"></canvas><script>const c=document.querySelector("canvas");c.getContext("2d").fillRect(5,5,90,90)</script>');
  await page.evaluate(() => document.fonts.ready);
  if (output) {
    await mkdir(output, { recursive: true });
    await page.screenshot({ path: resolve(output, 'creative-check.png') });
  }
} finally { await browser.close(); }
const run = (command, args) => {
  const result = spawnSync(command, args, { encoding: 'utf8' });
  if (result.status !== 0) throw new Error(`${command} check failed:\n${result.stderr?.slice(-8000) ?? result.error?.message ?? 'unknown process failure'}`);
  return result.stdout;
};
run('ffmpeg', ['-version']);
run('ffprobe', ['-version']);
if (!run('fc-match', ['-f', '%{family}', ':lang=zh-cn']).includes('Noto')) throw new Error('CJK font unavailable');
if (output) {
  run('ffmpeg', ['-nostdin', '-y', '-loop', '1', '-i', resolve(output, 'creative-check.png'), '-t', '1', '-r', '24', '-c:v', 'libx264', '-pix_fmt', 'yuv420p', resolve(output, 'creative-check.mp4')]);
  const media = JSON.parse(run('ffprobe', ['-v', 'error', '-show_streams', '-of', 'json', resolve(output, 'creative-check.mp4')]));
  if (!media.streams.some(s => s.codec_name === 'h264' && s.width === 640 && s.height === 360)) throw new Error('Video verification failed');
}
console.log(JSON.stringify({ ready: true, browser: chromium.executablePath(), output: output ?? null }));
