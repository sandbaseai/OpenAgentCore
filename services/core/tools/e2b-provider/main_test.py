"""Helper lifecycle diagnostics keep stdout authoritative and stderr bounded."""
import io
import json
import time
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import main
from state import Failure


class HelperTimingTest(unittest.TestCase):
    def run_helper(self, result=None, construction_error=None, execution_error=None, sink=None, stdout_sink=None):
        stdout, stderr = stdout_sink or io.StringIO(), sink or io.StringIO()
        with patch('main.sys.stdin', SimpleNamespace(buffer=io.BytesIO(b'{}'))), \
                patch('main.sys.stdout', stdout), patch('main.sys.stderr', stderr), \
                patch('main.sys.argv', ['helper']), patch.dict('main.os.environ', {}, clear=True), \
                patch('main.os.umask'), patch('main.logging.disable'), \
                patch('main.importlib.metadata.version', return_value=main.SDK_VERSION), \
                patch('main._HELPER_STARTED', time.monotonic_ns()), patch('main.Provider') as provider:
            provider.side_effect = construction_error
            provider.return_value.execute.side_effect = execution_error
            provider.return_value.execute.return_value = result or {'Version': main.PROTOCOL_VERSION}
            main.main()
            calls = provider.return_value.execute.call_count
        rows = [json.loads(line) for line in stderr.getvalue().splitlines()] if sink is None else []
        return json.loads(stdout.getvalue()), rows, calls

    def test_startup_execute_and_typed_error_preserve_response(self):
        for error in ('', 'unconfirmed'):
            with self.subTest(error=error):
                expected = {'Version': main.PROTOCOL_VERSION, 'ErrorCode': error}
                output, rows, calls = self.run_helper(result=expected)
                self.assertEqual(output, expected)
                self.assertEqual(calls, 1)
                self.assertEqual([r['stage'] for r in rows], ['helper_startup', 'helper_execute', 'helper_response_serialize', 'helper_stdout_write'])
                self.assertEqual([r['completed'] for r in rows], [True, not error, True, True])
                for row in rows:
                    self.assertEqual(set(row), {'event', 'stage', 'duration_us', 'completed'})
                    self.assertEqual(row['event'], 'e2b_helper_stage')
                    self.assertGreaterEqual(row['duration_us'], 0)
                    self.assertLessEqual(row['duration_us'], 1_800_000_000)

    def test_failures_emit_only_completed_boundaries_without_error_text(self):
        for phase in ('startup', 'execute'):
            with self.subTest(phase=phase):
                args = {'construction_error': Failure('invalid')} if phase == 'startup' else {'execution_error': RuntimeError('SECRET SDK body')}
                output, rows, calls = self.run_helper(**args)
                self.assertEqual(output['ErrorCode'], 'invalid' if phase == 'startup' else 'unconfirmed')
                self.assertEqual(calls, 0 if phase == 'startup' else 1)
                self.assertEqual(rows[-3]['stage'], 'helper_' + phase)
                self.assertFalse(rows[-3]['completed'])
                self.assertNotIn('SECRET', json.dumps(rows))

    def test_diagnostic_sink_failure_never_replays_or_changes_stdout(self):
        class BrokenSink:
            def write(self, _):
                raise OSError('private sink details')
        output, _, calls = self.run_helper(sink=BrokenSink())
        self.assertEqual(output, {'Version': main.PROTOCOL_VERSION})
        self.assertEqual(calls, 1)

    def test_timing_uses_local_monotonic_clock(self):
        sink = io.StringIO()
        with patch('main.sys.stderr', sink), patch('main.time.monotonic_ns', return_value=2501000):
            main.helper_stage('helper_execute', 1000, True)
        self.assertEqual(json.loads(sink.getvalue())['duration_us'], 2500)

    def test_stdout_bytes_unchanged_and_write_does_not_flush(self):
        class SlowOutput(io.StringIO):
            def write(self, value):
                time.sleep(0.02)
                return super().write(value)
            def flush(self):
                raise AssertionError('unexpected flush')
        sink = SlowOutput()
        expected = {'Version': main.PROTOCOL_VERSION, 'ErrorCode': ''}
        _, rows, calls = self.run_helper(result=expected, stdout_sink=sink)
        self.assertEqual(sink.getvalue(), json.dumps(expected) + '\n')
        self.assertEqual(calls, 1)
        self.assertGreaterEqual(rows[-1]['duration_us'], 15000)
        self.assertTrue(rows[-1]['completed'])

    def test_stdout_failure_propagates_and_records_failed_write(self):
        class BrokenOutput(io.StringIO):
            def write(self, value):
                raise BrokenPipeError('SECRET output failure')
        diagnostics = io.StringIO()
        with self.assertRaises(BrokenPipeError):
            self.run_helper(stdout_sink=BrokenOutput(), sink=diagnostics)
        rows = [json.loads(line) for line in diagnostics.getvalue().splitlines()]
        self.assertEqual(rows[-1]['stage'], 'helper_stdout_write')
        self.assertFalse(rows[-1]['completed'])
        self.assertNotIn('SECRET', diagnostics.getvalue())

    def test_serialization_failure_propagates_without_inventing_write(self):
        diagnostics = io.StringIO()
        output = io.StringIO()
        with self.assertRaises(TypeError):
            self.run_helper(result={'Version': main.PROTOCOL_VERSION, 'Command': {1}},
                            sink=diagnostics, stdout_sink=output)
        rows = [json.loads(line) for line in diagnostics.getvalue().splitlines()]
        self.assertEqual(rows[-1]['stage'], 'helper_response_serialize')
        self.assertFalse(rows[-1]['completed'])
        self.assertNotIn('helper_stdout_write', [r['stage'] for r in rows])
        self.assertEqual(output.getvalue(), '')

    def test_invalid_response_keeps_existing_fallback_bytes(self):
        output = io.StringIO()
        result, rows, calls = self.run_helper(result={'Version': main.PROTOCOL_VERSION, 'Unknown': 'SECRET'}, stdout_sink=output)
        expected = {'Version': main.PROTOCOL_VERSION, 'ErrorCode': 'unconfirmed'}
        self.assertEqual(output.getvalue(), json.dumps(expected) + '\n')
        self.assertEqual(result, expected)
        self.assertEqual(calls, 1)
        self.assertTrue(rows[-2]['completed'])
        self.assertTrue(rows[-1]['completed'])
        self.assertNotIn('SECRET', json.dumps(rows))

    def test_serialization_delay_is_separate_from_write(self):
        original = json.dumps
        expected = {'Version': main.PROTOCOL_VERSION}
        def slow_response(value, *args, **kwargs):
            if value is expected:
                time.sleep(0.02)
            return original(value, *args, **kwargs)
        with patch('main.json.dumps', side_effect=slow_response):
            _, rows, calls = self.run_helper(result=expected)
        self.assertEqual(calls, 1)
        self.assertGreaterEqual(rows[-2]['duration_us'], 15000)
        self.assertEqual(rows[-2]['stage'], 'helper_response_serialize')
        self.assertEqual(rows[-1]['stage'], 'helper_stdout_write')
