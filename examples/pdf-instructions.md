# Remote PDF example

Create one clean, readable page named outputs/remote-report.pdf.

Title: ai-mesh remote PDF example

Include:
- A short explanation that the input and task brief came from another computer.
- The actual hostname of the computer generating the PDF.
- The exact context marker supplied in the separate task brief.
- A note that Mesh will collect the finished files on the submitting computer.

Use Python 3 and the standard library to write a valid PDF with Helvetica text,
a correct xref table and appropriate margins. Use no extra PDF packages.
Put the generator in outputs/generate_pdf.py so the generation can
be reproduced. Execute it here and also write the output of hostname to
outputs/generated-on.txt. Do not delegate and do not install packages.
