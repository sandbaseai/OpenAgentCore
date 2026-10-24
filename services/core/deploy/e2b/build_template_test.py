"""Exercise real Runtime tar metadata without Docker or provider calls."""
import argparse
import ast
import contextlib
import io
import itertools
import json
import os
from pathlib import Path
import runpy
import stat
import subprocess
import sys
import tarfile
import tempfile
import textwrap
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch


class BundlePermissionsTest(unittest.TestCase):
    def test_public_parents_and_runtime_modes_under_both_umasks(self):
        for mask, creative, profile in itertools.product((0o022, 0o077), (False, True), (None, '2c8g', '4c8g')):
            expected_cpu, expected_memory = (4 if profile == '4c8g' else 2), (8192 if profile else 2048)
            with self.subTest(umask=oct(mask), creative=creative), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                key = root / 'key'
                key.write_text('fixture-only')
                key.chmod(0o600)
                output = root / 'private/result.json'
                source_modes = {'bin': 0o755, 'bin/daemon': 0o751,
                                'codex-resources': 0o755, 'codex': 0o700,
                                'codex/config': 0o600, 'opt': 0o755,
                                'opt/private': 0o700, 'opt/private/key': 0o600}
                if creative:
                    source_modes.update({'opt/oac-creative': 0o755, 'opt/oac-creative/install.sh': 0o755})
                image = {'Architecture': 'amd64', 'Os': 'linux', 'Id': 'sha256:fixture',
                         'Config': {'Env': ['OAC_RUNTIME_WORKSPACE=/environment/workspace']}}

                def check_output(argv, **kwargs):
                    if argv == ['docker', 'image', 'inspect', 'sha256:fixture']:
                        return json.dumps([image]).encode()
                    self.assertEqual(argv, ['docker', 'create', 'sha256:fixture'])
                    return 'fixture-container\n'

                def run(argv, **kwargs):
                    if argv == ['docker', 'rm', 'fixture-container']:
                        return SimpleNamespace(returncode=0)
                    self.assertEqual(argv[:2], ['docker', 'cp'])
                    self.assertEqual(argv[-1], '-')
                    name = argv[2].split(':', 1)[1].rsplit('/', 1)[-1]
                    with tarfile.open(fileobj=kwargs['stdout'], mode='w') as archive:
                        for path, mode in source_modes.items():
                            if path != name and not path.startswith(name + '/'):
                                continue
                            entry = tarfile.TarInfo(path)
                            entry.mode = mode
                            if path in ('bin', 'codex-resources', 'codex', 'opt', 'opt/private', 'opt/oac-creative'):
                                entry.type = tarfile.DIRTYPE
                                archive.addfile(entry)
                            else:
                                entry.size = 7
                                archive.addfile(entry, io.BytesIO(b'fixture'))
                    return SimpleNamespace(returncode=0)

                template = Mock()
                for method in ('from_image', 'run_cmd', 'copy', 'set_user', 'set_workdir'):
                    getattr(template, method).return_value = template

                def build(instance, **kwargs):
                    self.assertIs(instance, template)
                    self.assertEqual((kwargs['cpu_count'], kwargs['memory_mb']), (expected_cpu, expected_memory))
                    context = Path(factory.call_args.kwargs['file_context_path'])
                    self.assertEqual(stat.S_IMODE(context.stat().st_mode), 0o700)
                    with tarfile.open(context / 'runtime.tar.gz') as archive:
                        modes = {m.name: stat.S_IMODE(m.mode) for m in archive.getmembers()}
                    for parent in ('usr', 'usr/local', 'etc'):
                        self.assertEqual(modes[parent], 0o755)
                    for path, mode in source_modes.items():
                        prefix = 'usr/local/' if path.split('/')[0] in ('bin', 'codex-resources') else 'etc/' if path.startswith('codex') else ''
                        self.assertEqual(modes[prefix + path], mode)
                    self.assertEqual(stat.S_IMODE((context / 'runtime.tar.gz').stat().st_mode), 0o666 & ~mask)
                    self.assertEqual(stat.S_IMODE(key.stat().st_mode), 0o600)
                    projection = 'helper_contract_generated.py'
                    self.assertEqual((context / projection).read_bytes(), Path(__file__).with_name(projection).read_bytes())
                    for source in ('init.py', 'managed_init.py', projection):
                        template.copy.assert_any_call(source, '/opt/oac-e2b/' + source, user='root')
                    self.assertTrue(any('chmod 0555' in call.args[0] and '/opt/oac-e2b/' + projection in call.args[0]
                                        for call in template.run_cmd.call_args_list))
                    commands = [call.args[0] for call in template.run_cmd.call_args_list]
                    self.assertEqual('sh /opt/oac-creative/install.sh --system-only' in commands, creative)
                    return SimpleNamespace(template_id='fixture', build_id='build')

                factory = Mock(return_value=template)
                factory.build.side_effect = build
                original_mask = os.umask(mask)
                try:
                    with patch.dict(sys.modules, {'e2b': SimpleNamespace(Template=factory)}), \
                            patch.dict(os.environ, {'OAC_DEV_HOME': str(root / 'state')}), \
                            patch.object(sys, 'argv', ['build-template.py', '--image', 'sha256:fixture',
                                '--name', 'fixture', '--api-key-file', str(key), '--output', str(output)] +
                                (['--compute-profile', profile] if profile else [])), \
                            patch('subprocess.check_output', side_effect=check_output), \
                            patch('subprocess.run', side_effect=run), contextlib.redirect_stdout(io.StringIO()):
                        runpy.run_path(str(Path(__file__).with_name('build-template.py')), run_name='__main__')
                    self.assertEqual(os.umask(mask), mask)
                    self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o666 & ~mask)
                    self.assertEqual(stat.S_IMODE(output.parent.stat().st_mode), 0o777 & ~mask)
                    self.assertEqual(key.read_text(), 'fixture-only')
                    self.assertEqual(json.loads(output.read_text())['creative_tools'], creative)
                    factory.build.assert_called_once()
                    report = json.loads(output.read_text())
                    self.assertEqual((report['cpu_count'], report['memory_mb']), (expected_cpu, expected_memory))
                    self.assertEqual(report['compute_profile'], profile or '2c2g')
                finally:
                    os.umask(original_mask)


class ComputeProfileTest(unittest.TestCase):
    def test_invalid_profile_fails_before_docker_or_provider(self):
        for profile in ('', '4c2g', '8c8g', '4c8g;command'):
            factory = Mock()
            with self.subTest(profile=profile), patch.dict(sys.modules, {'e2b': SimpleNamespace(Template=factory)}), \
                    patch.object(sys, 'argv', ['build-template.py', '--image', 'sha256:fixture',
                        '--name', 'fixture', '--api-key-file', '/private/key', '--output', '/private/result',
                        '--compute-profile', profile]), patch('subprocess.check_output') as docker, \
                    contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as error:
                runpy.run_path(str(Path(__file__).with_name('build-template.py')), run_name='__main__')
            self.assertEqual(error.exception.code, 2)
            docker.assert_not_called()
            factory.assert_not_called()

    def test_workflow_checks_fresh_and_resumed_build_resources(self):
        workflow = Path(__file__).resolve().parents[4] / '.github/workflows/e2b-template.yml'
        source = workflow.read_text()
        start = source.index('          def request(client, **kwargs):')
        end = source.index('          sdk.request_build = request', start)
        function = compile(ast.parse(textwrap.dedent(source[start:end])), str(workflow), 'exec')
        cases = (
            ('valid', [{'buildID': 'build', 'cpuCount': 4, 'memoryMB': 8192}]),
            ('wrong-resource', [{'buildID': 'build', 'cpuCount': 2, 'memoryMB': 2048}]),
            ('missing-memory', [{'buildID': 'build', 'cpuCount': 4}]),
            ('non-integer', [{'buildID': 'build', 'cpuCount': True, 'memoryMB': 8192}]),
            ('missing-build', []),
            ('different-build', [{'buildID': 'other', 'cpuCount': 4, 'memoryMB': 8192}]),
            ('duplicate', [{'buildID': 'build', 'cpuCount': 4, 'memoryMB': 8192}] * 2),
            ('default', [{'buildID': 'build', 'cpuCount': 2, 'memoryMB': 2048}]),
        )
        for resumed, (name, builds) in itertools.product((False, True), cases):
            with self.subTest(resumed=resumed, case=name), tempfile.TemporaryDirectory() as temporary:
                output = Path(temporary)
                client = Mock()
                status = {'templateID': 'template', 'buildID': 'build', 'status': 'waiting', 'logs': []}
                detail = {'templateID': 'template', 'builds': builds}
                responses = [Mock(json=Mock(return_value=detail))]
                if resumed:
                    responses.insert(0, Mock(json=Mock(return_value=status)))
                client.get_httpx_client.return_value.get.side_effect = responses
                original = Mock(return_value=SimpleNamespace(template_id='template', build_id='build'))
                namespace = dict(os=os, json=json, SimpleNamespace=SimpleNamespace,
                                 original_request=original, output=output)
                exec(function, namespace)
                resources = (2, 2048) if name == 'default' else (4, 8192)
                with patch.dict(os.environ, {'RESUME_BUILD': 'template:build' if resumed else '',
                                             'SOURCE_COMMIT': 'fixture', 'E2B_API_URL': 'https://fixture.invalid'}):
                    if name in ('valid', 'default'):
                        namespace['request'](client, cpu_count=resources[0], memory_mb=resources[1])
                        receipt = json.loads((output / 'build-request.json').read_text())
                        self.assertEqual((receipt['cpu_count'], receipt['memory_mb']), resources)
                    else:
                        with self.assertRaisesRegex(RuntimeError, 'CPU/memory'):
                            namespace['request'](client, cpu_count=4, memory_mb=8192)
                        self.assertFalse(json.loads((output / 'build-request.json').read_text())['resources_verified'])
                self.assertEqual(original.call_count, 0 if resumed else 1)

    def test_default_omits_optional_flag_for_historical_source_parser(self):
        workflow = Path(__file__).resolve().parents[4] / '.github/workflows/e2b-template.yml'
        source = workflow.read_text()
        start = source.index('          compute_args=()')
        end = source.index('          esac', start) + len('          esac')
        script = textwrap.dedent(source[start:end]) + '\nif (( ${#compute_args[@]} )); then printf "%s\\0" "${compute_args[@]}"; fi'
        for profile in ('2c2g', '2c8g', '4c8g'):
            result = subprocess.run(['bash', '-c', script], env={**os.environ, 'COMPUTE_PROFILE': profile},
                                    capture_output=True, check=True)
            args = result.stdout.decode().split('\0')[:-1]
            old_parser = argparse.ArgumentParser()
            if profile == '2c2g':
                self.assertEqual(vars(old_parser.parse_args(args)), {})
            else:
                self.assertEqual(args, ['--compute-profile', profile])
                with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                    old_parser.parse_args(args)

    def test_report_uses_verified_receipt_for_old_builder_and_rejects_mismatch(self):
        workflow = Path(__file__).resolve().parents[4] / '.github/workflows/e2b-template.yml'
        source = workflow.read_text()
        start = source.index("          value = json.loads(report.read_text())")
        end = source.index('          value.update(source_commit=', start)
        code = compile(textwrap.dedent(source[start:end]), str(workflow), 'exec')
        for verified, template in itertools.product((True, False), ('template:build', 'template:other')):
            with self.subTest(verified=verified, template=template), tempfile.TemporaryDirectory() as temporary:
                report = Path(temporary) / 'template.json'
                report.write_text(json.dumps({'template': 'template:build', 'image': 'fixture'}))
                report.with_name('build-request.json').write_text(json.dumps({
                    'template': template, 'resources_verified': verified, 'cpu_count': 2, 'memory_mb': 2048}))
                namespace = {'json': json, 'report': report}
                if verified and template == 'template:build':
                    exec(code, namespace)
                    self.assertEqual((namespace['value']['cpu_count'], namespace['value']['memory_mb']), (2, 2048))
                else:
                    with self.assertRaisesRegex(RuntimeError, 'verified allocation receipt'):
                        exec(code, namespace)


if __name__ == '__main__':
    unittest.main()
