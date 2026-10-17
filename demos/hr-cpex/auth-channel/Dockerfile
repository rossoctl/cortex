FROM python:3.12-slim

WORKDIR /app

COPY requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r requirements.txt

COPY server.py /app/server.py

EXPOSE 5001

CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "5001"]
