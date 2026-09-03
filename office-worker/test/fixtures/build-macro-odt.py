#!/usr/bin/env python3
"""Builds test/fixtures/macro.odt: a soffice-free (pure zipfile) ODF text
document with an embedded StarBasic macro named "AutoOpen" — the classic
auto-executing document-macro convention (the same naming LibreOffice honors
for legacy .doc "AutoOpen"/"AutoExec" macros). The macro overwrites the
document body text ("ORIGINAL_TEXT_MARKER" -> "MACRO_RAN") and re-saves, so a
consumer of office.convert's output can tell whether the macro fired by
checking which string survived — "prove the macro didn't run... or alter
output" per 's CP3 success criterion.

Verified empirically (LibreOffice 7.4.7.2, via the office-worker Docker
image) against both an unhardened profile and this worker's hardened
per-job profile (src/handlers/soffice.js): `soffice --convert-to` batch
conversion leaves ORIGINAL_TEXT_MARKER intact either way — LibreOffice's
own batch-conversion path already refuses macro execution unconditionally,
independent of the MacroSecurityLevel registry setting. The libreoffice
CP3 test therefore asserts the (always-true-today) safe outcome as a
regression guard, not as proof the registry hardening alone is what's
blocking it — see soffice.js's comment for the full disposition.

Regenerate: `python3 build-macro-odt.py macro.odt`
"""
import sys
import zipfile

out = sys.argv[1]

MANIFEST = '''<?xml version="1.0" encoding="UTF-8"?>
<manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0" manifest:version="1.2">
 <manifest:file-entry manifest:full-path="/" manifest:version="1.2" manifest:media-type="application/vnd.oasis.opendocument.text"/>
 <manifest:file-entry manifest:full-path="content.xml" manifest:media-type="text/xml"/>
 <manifest:file-entry manifest:full-path="styles.xml" manifest:media-type="text/xml"/>
 <manifest:file-entry manifest:full-path="meta.xml" manifest:media-type="text/xml"/>
 <manifest:file-entry manifest:full-path="settings.xml" manifest:media-type="text/xml"/>
 <manifest:file-entry manifest:full-path="Basic/" manifest:media-type=""/>
 <manifest:file-entry manifest:full-path="Basic/script-lb.xml" manifest:media-type="text/xml"/>
 <manifest:file-entry manifest:full-path="Basic/Standard/" manifest:media-type=""/>
 <manifest:file-entry manifest:full-path="Basic/Standard/script-lb.xml" manifest:media-type="text/xml"/>
 <manifest:file-entry manifest:full-path="Basic/Standard/Module1.xml" manifest:media-type="text/xml"/>
</manifest:manifest>
'''

# No explicit event-listener this time -- rely purely on the AutoOpen naming
# convention (the classic macro-virus vector for auto-executing document macros).
CONTENT = '''<?xml version="1.0" encoding="UTF-8"?>
<office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0"
 xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0"
 office:version="1.2">
 <office:automatic-styles/>
 <office:body>
  <office:text>
   <text:p>ORIGINAL_TEXT_MARKER</text:p>
  </office:text>
 </office:body>
</office:document-content>
'''

STYLES = '''<?xml version="1.0" encoding="UTF-8"?>
<office:document-styles xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" office:version="1.2">
 <office:styles/>
</office:document-styles>
'''

META = '''<?xml version="1.0" encoding="UTF-8"?>
<office:document-meta xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" office:version="1.2">
 <office:meta/>
</office:document-meta>
'''

SETTINGS = '''<?xml version="1.0" encoding="UTF-8"?>
<office:document-settings xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" office:version="1.2">
 <office:settings/>
</office:document-settings>
'''

SCRIPT_LB_ROOT = '''<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE library:libraries PUBLIC "-//OpenOffice.org//DTD OfficeDocument 1.0//EN" "libraries.dtd">
<library:libraries xmlns:library="http://openoffice.org/2000/library" xmlns:xlink="http://www.w3.org/1999/xlink">
 <library:library library:name="Standard" library:link="false" library:embedded="true"/>
</library:libraries>
'''

SCRIPT_LB_STANDARD = '''<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE library:library PUBLIC "-//OpenOffice.org//DTD OfficeDocument 1.0//EN" "library.dtd">
<library:library xmlns:library="http://openoffice.org/2000/library" library:name="Standard"
 library:readonly="false" library:passwordprotected="false">
 <library:element library:name="Module1"/>
</library:library>
'''

MODULE1 = '''<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE script:module PUBLIC "-//OpenOffice.org//DTD OfficeDocument 1.0//EN" "module.dtd">
<script:module xmlns:script="http://openoffice.org/2000/script" script:name="Module1" script:language="StarBasic" script:moduleType="normal">
Sub AutoOpen
  Dim oDoc As Object
  oDoc = ThisComponent
  oDoc.getText().setString(&quot;MACRO_RAN&quot;)
  oDoc.store()
End Sub
</script:module>
'''

with zipfile.ZipFile(out, 'w') as z:
    z.writestr(zipfile.ZipInfo('mimetype'), 'application/vnd.oasis.opendocument.text', zipfile.ZIP_STORED)
    z.writestr('META-INF/manifest.xml', MANIFEST)
    z.writestr('content.xml', CONTENT)
    z.writestr('styles.xml', STYLES)
    z.writestr('meta.xml', META)
    z.writestr('settings.xml', SETTINGS)
    z.writestr('Basic/script-lb.xml', SCRIPT_LB_ROOT)
    z.writestr('Basic/Standard/script-lb.xml', SCRIPT_LB_STANDARD)
    z.writestr('Basic/Standard/Module1.xml', MODULE1)

print("wrote", out)
