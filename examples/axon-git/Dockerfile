FROM ghcr.io/cortexapps/cortex-axon-agent:latest
WORKDIR /project
COPY requirements.txt .
ENV VIRTUAL_ENV=/project/.venv
RUN python3 -m venv $VIRTUAL_ENV
ENV PATH="$VIRTUAL_ENV/bin:$PATH"

# Install Python dependencies
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
RUN echo "python main.py" > /project/run.sh
RUN chmod +x /project/run.sh
ENV PYTHONUNBUFFERED=1 

ENTRYPOINT [ "/app/app_entrypoint.sh", "/project/run.sh" ]
