# Remote PDF acceptance test

Create one clean, readable page named outputs/remote-report.pdf.

Title: ai-mesh remote PDF test

Include these exact statements:
- Instructions originated on the Mac.
- PDF generated on green-lighthouse.
- Result returned to the Mac.
- Context marker: PDF_HANDOFF_42

Use Python 3 and the standard library to write a valid PDF with Helvetica text,
a correct xref table and appropriate margins. There are no extra PDF packages
installed. Put the generator in outputs/generate_pdf.py so the generation can
be reproduced. Execute it here and also write the output of hostname to
outputs/generated-on.txt. Do not delegate and do not install packages.
