import io,json,unittest
from unittest.mock import patch
from provider_test import ProviderTest
class IndependentSDKQA(ProviderTest):
 def test_qa_read_failure_boundaries(self):
  for mode in ('call','enter','exit'):
   with self.subTest(mode=mode):
    if mode=='call': self.cloud.files.read.side_effect=TimeoutError('QA-CANARY')
    if mode=='enter':
     self.cloud.files.read.side_effect=lambda *a,**k:self.template_stream
     self.template_stream.__enter__.side_effect=TimeoutError('QA-CANARY')
    if mode=='exit':
     self.template_stream.__enter__.side_effect=None
     self.template_stream.__exit__.side_effect=TimeoutError('QA-CANARY')
    # Fresh receipt for each attempt; this fixture never contacts E2B.
    for file in __import__('pathlib').Path(self.temporary.name).glob('*.json'):file.unlink()
    stream=io.StringIO()
    with patch('provider.sys.stderr',stream):self.assertEqual(self.call('create')['ErrorCode'],'unconfirmed')
    rows=[json.loads(x) for x in stream.getvalue().splitlines()]
    stages={x['stage']:x for x in rows}
    self.assertTrue(stages['template_get_info']['completed'])
    self.assertFalse(stages['template_read_check']['completed'])
    self.assertFalse(stages['template_check']['completed'])
    self.assertEqual(sum(x['stage']=='template_read_check' for x in rows),1)
    self.assertNotIn('bootstrap_write',stages)
    self.assertNotIn('QA-CANARY',stream.getvalue())
if __name__=='__main__': unittest.main(defaultTest='IndependentSDKQA.test_qa_read_failure_boundaries')
