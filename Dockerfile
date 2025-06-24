FROM public.ecr.aws/l9h5y2v4/netmind_inference_public:frps

COPY ./release/frps_linux_amd64 /usr/bin/frps
