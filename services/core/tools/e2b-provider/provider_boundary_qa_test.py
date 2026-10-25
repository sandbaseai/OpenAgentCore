"""Template probe failures must not write credentials or expose native output."""
import io
import json
from unittest.mock import patch
from provider_test import Fixture
from state import Failure


class IndependentSDKQA(Fixture):
    def test_template_probe_failure_is_bounded(self):
        for result in (Failure('command_unconfirmed'), {'ExitCode': 78, 'Stderr': 'QA-CANARY'}):
            with self.subTest(result=type(result).__name__):
                for file in __import__('pathlib').Path(self.temporary.name).glob('*.json'):
                    file.unlink()
                output = io.StringIO()
                probe = patch('provider.run', side_effect=result) if isinstance(result, Exception) else patch('provider.run', return_value=result)
                with probe, patch('provider.sys.stderr', output):
                    self.assertIn(self.call('create')['ErrorCode'], ('command_unconfirmed', 'template_invalid'))
                rows = [json.loads(line) for line in output.getvalue().splitlines()]
                stages = {row['stage']: row for row in rows}
                self.assertFalse(stages['template_check']['completed'])
                self.assertNotIn('bootstrap_write', stages)
                self.cloud.files.write.assert_not_called()
                self.assertNotIn('QA-CANARY', output.getvalue())
